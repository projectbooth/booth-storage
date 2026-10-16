package seaweedfslease

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// isMalformedID reports a Postgres "invalid input syntax for type uuid" error: the id
// column is UUID, so a caller-supplied id that was never a valid UUID can never match
// any row — the same outcome as a well-formed id that simply isn't there (mirrors
// internal/uploadgrant.isMalformedID).
func isMalformedID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// PostgresStore is the Store backed by the platform's shared PostgreSQL cluster (ADR
// 0014), sharing booth-storage's one connection pool and migration runner
// (registry.PostgresStore.Pool) rather than opening a second pool for one more table.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore wraps an already-migrated pool. The storage_seaweedfs_leases table is
// one of this module's own migrations (internal/registry/migrations), applied once at
// boot regardless of which store this wraps.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func (s *PostgresStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

const leaseCols = `id, workspace, backend_id, iam_user_name, access_key_id, prefix, access, expires_at, created_at, swept_at`

func scanLease(row pgx.Row) (Lease, error) {
	var l Lease
	if err := row.Scan(&l.ID, &l.Workspace, &l.BackendID, &l.IAMUserName, &l.AccessKeyID, &l.Prefix, &l.Access, &l.ExpiresAt, &l.CreatedAt, &l.SweptAt); err != nil {
		return Lease{}, err
	}
	l.ExpiresAt, l.CreatedAt = l.ExpiresAt.UTC(), l.CreatedAt.UTC()
	if l.SweptAt != nil {
		t := l.SweptAt.UTC()
		l.SweptAt = &t
	}
	return l, nil
}

func (s *PostgresStore) Record(ctx context.Context, l Lease) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO storage_seaweedfs_leases (`+leaseCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		l.ID, l.Workspace, l.BackendID, l.IAMUserName, l.AccessKeyID, l.Prefix, l.Access, l.ExpiresAt, l.CreatedAt, l.SweptAt)
	return err
}

func (s *PostgresStore) PendingSweep(ctx context.Context, now time.Time, limit int) ([]Lease, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+leaseCols+` FROM storage_seaweedfs_leases
		WHERE swept_at IS NULL AND expires_at < $1
		ORDER BY expires_at ASC LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Lease{}
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *PostgresStore) MarkSwept(ctx context.Context, id string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE storage_seaweedfs_leases SET swept_at = $1 WHERE id = $2`, at, id)
	if isMalformedID(err) {
		return nil
	}
	return err
}

func (s *PostgresStore) ActiveCount(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM storage_seaweedfs_leases WHERE swept_at IS NULL AND expires_at >= $1`, now).Scan(&n)
	return n, err
}
