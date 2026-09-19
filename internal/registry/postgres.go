package registry

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/projectbooth/booth-storage/internal/backend"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID is an arbitrary constant identifying booth-storage's migration lock
// in Postgres's advisory-lock keyspace, so replicas starting at the same time
// serialize instead of racing to create the same table.
const migrationLockID int64 = 0x626f6f7473746f72 // "bootstor"

// PostgresStore is the MetadataStore backed by the platform's shared PostgreSQL
// cluster (ADR 0014).
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore connects to dsn and applies any pending migrations.
func NewPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	s := &PostgresStore{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the connection pool.
func (s *PostgresStore) Close() { s.pool.Close() }

// migrate applies embedded migrations in filename order, each exactly once, all inside
// one transaction holding an advisory lock — safe to run from every replica at boot.
func (s *PostgresStore) migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting migration transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("taking migration lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS storage_schema_migrations (version INT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("creating migrations table: %w", err)
	}

	for _, name := range names {
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %q: filename must start with a numeric version", name)
		}
		var applied bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM storage_schema_migrations WHERE version = $1)`, version).Scan(&applied); err != nil {
			return fmt.Errorf("checking migration %s: %w", name, err)
		}
		if applied {
			continue
		}
		sql, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("applying migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO storage_schema_migrations (version) VALUES ($1)`, version); err != nil {
			return fmt.Errorf("recording migration %s: %w", name, err)
		}
	}
	return tx.Commit(ctx)
}

const selectCols = `workspace, id, display_name, kind, config, has_credentials, created_by, created_at, updated_at`

func scan(row pgx.Row) (Record, error) {
	var r Record
	var kind string
	if err := row.Scan(&r.Workspace, &r.ID, &r.DisplayName, &kind, &r.Config, &r.HasCredentials, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return Record{}, err
	}
	r.Kind = backend.Kind(kind)
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	return r, nil
}

func (s *PostgresStore) Create(ctx context.Context, r Record) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO storage_backends (`+selectCols+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		r.Workspace, r.ID, r.DisplayName, string(r.Kind), []byte(r.Config), r.HasCredentials, r.CreatedBy, r.CreatedAt, r.UpdatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return ErrExists
	}
	return err
}

func (s *PostgresStore) Get(ctx context.Context, workspace, id string) (Record, error) {
	r, err := scan(s.pool.QueryRow(ctx, `SELECT `+selectCols+` FROM storage_backends WHERE workspace = $1 AND id = $2`, workspace, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	return r, err
}

func (s *PostgresStore) List(ctx context.Context, workspace string) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+selectCols+` FROM storage_backends WHERE workspace = $1 ORDER BY id`, workspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Update(ctx context.Context, r Record) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE storage_backends SET display_name=$3, config=$4, has_credentials=$5, updated_at=$6 WHERE workspace=$1 AND id=$2`,
		r.Workspace, r.ID, r.DisplayName, []byte(r.Config), r.HasCredentials, r.UpdatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) Delete(ctx context.Context, workspace, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM storage_backends WHERE workspace=$1 AND id=$2`, workspace, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
