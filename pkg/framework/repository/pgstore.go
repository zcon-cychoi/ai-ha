package repository

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// Migrate creates the repository table if it does not exist.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply repository schema: %w", err)
	}
	return nil
}

// PGStore is a Store backed by PostgreSQL.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore returns a Store using pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

const (
	uniqueViolation = "23505"
	nameConstraint  = "repositories_name_key"
)

const insertSQL = `
INSERT INTO repositories (repository_id, name, type, provider, endpoint, region, bucket, prefix,
                          credentials, has_session_token)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

// Insert implements Store.
func (s *PGStore) Insert(ctx context.Context, r Record) error {
	d := r.Definition
	_, err := s.pool.Exec(ctx, insertSQL, r.ID, d.Name, r.Type, d.Provider, d.Endpoint, d.Region,
		d.Bucket, d.Prefix, r.Credentials, r.HasSessionToken)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == nameConstraint {
		return ErrExists
	}
	if err != nil {
		return fmt.Errorf("insert repository: %w", err)
	}
	return nil
}

const columns = `repository_id, name, type, provider, endpoint, region, bucket, prefix,
       credentials, has_session_token, created_at, credentials_updated_at`

// Get implements Store. An ID is matched before a name; names are refused
// when they have the shape of an ID, so the two never collide.
func (s *PGStore) Get(ctx context.Context, idOrName string) (Record, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+columns+` FROM repositories
		WHERE repository_id = $1 OR name = $1
		ORDER BY (repository_id = $1) DESC LIMIT 1`, idOrName)
	return scan(row)
}

// List implements Store.
func (s *PGStore) List(ctx context.Context) ([]Record, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM repositories ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	return out, nil
}

// SetCredentials implements Store.
func (s *PGStore) SetCredentials(ctx context.Context, id string, sealed []byte, hasSessionToken bool) (Record, error) {
	row := s.pool.QueryRow(ctx, `UPDATE repositories
		SET credentials = $2, has_session_token = $3, credentials_updated_at = now()
		WHERE repository_id = $1 RETURNING `+columns, id, sealed, hasSessionToken)
	return scan(row)
}

func scan(row pgx.Row) (Record, error) {
	var r Record
	d := &r.Definition
	err := row.Scan(&r.ID, &d.Name, &r.Type, &d.Provider, &d.Endpoint, &d.Region, &d.Bucket, &d.Prefix,
		&r.Credentials, &r.HasSessionToken, &r.CreatedAt, &r.CredentialsUpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("read repository: %w", err)
	}
	return r, nil
}
