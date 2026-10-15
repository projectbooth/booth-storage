package uploadgrant

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// isMalformedID reports a Postgres "invalid input syntax for type uuid" error: the id
// column is UUID, so a caller-supplied id that was never a valid UUID (an admin API
// client guessing, a forged path parameter) can never match any row — the same outcome
// as a well-formed id that simply isn't there.
func isMalformedID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// PostgresStore is the Store backed by the platform's shared PostgreSQL cluster
// (ADR 0014), sharing booth-storage's one connection pool and migration runner
// (registry.PostgresStore.Pool) rather than opening a second pool for one more table.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore wraps an already-migrated pool. The storage_upload_grants table is
// one of this module's own migrations (internal/registry/migrations), applied once at
// boot regardless of which store this wraps.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func (s *PostgresStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

const selectCols = `id, workspace, principal, backend_id, prefix, extensions, max_bytes, granted_by, created_at`

func scan(row pgx.Row) (Grant, error) {
	var g Grant
	if err := row.Scan(&g.ID, &g.Workspace, &g.Principal, &g.BackendID, &g.Prefix, &g.Extensions, &g.MaxBytes, &g.GrantedBy, &g.CreatedAt); err != nil {
		return Grant{}, err
	}
	g.CreatedAt = g.CreatedAt.UTC()
	return g, nil
}

func (s *PostgresStore) Upsert(ctx context.Context, g Grant) (Grant, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO storage_upload_grants (`+selectCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (workspace, principal, backend_id, prefix) DO UPDATE SET
			id = EXCLUDED.id, extensions = EXCLUDED.extensions, max_bytes = EXCLUDED.max_bytes,
			granted_by = EXCLUDED.granted_by, created_at = EXCLUDED.created_at
		RETURNING `+selectCols,
		g.ID, g.Workspace, g.Principal, g.BackendID, g.Prefix, g.Extensions, g.MaxBytes, g.GrantedBy, g.CreatedAt)
	return scan(row)
}

func (s *PostgresStore) Get(ctx context.Context, workspace, id string) (Grant, error) {
	g, err := scan(s.pool.QueryRow(ctx, `SELECT `+selectCols+` FROM storage_upload_grants WHERE workspace=$1 AND id=$2`, workspace, id))
	if errors.Is(err, pgx.ErrNoRows) || isMalformedID(err) {
		return Grant{}, ErrNotFound
	}
	return g, err
}

func (s *PostgresStore) List(ctx context.Context, workspace, principal string) ([]Grant, error) {
	var rows pgx.Rows
	var err error
	if principal == "" {
		rows, err = s.pool.Query(ctx, `SELECT `+selectCols+` FROM storage_upload_grants WHERE workspace=$1 ORDER BY id`, workspace)
	} else {
		rows, err = s.pool.Query(ctx, `SELECT `+selectCols+` FROM storage_upload_grants WHERE workspace=$1 AND principal=$2 ORDER BY id`, workspace, principal)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Grant{}
	for rows.Next() {
		g, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Delete(ctx context.Context, workspace, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM storage_upload_grants WHERE workspace=$1 AND id=$2`, workspace, id)
	if err != nil {
		if isMalformedID(err) {
			return ErrNotFound
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) DeleteByPrincipal(ctx context.Context, workspace, principal string) ([]Grant, error) {
	rows, err := s.pool.Query(ctx, `DELETE FROM storage_upload_grants WHERE workspace=$1 AND principal=$2 RETURNING `+selectCols, workspace, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Grant{}
	for rows.Next() {
		g, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
