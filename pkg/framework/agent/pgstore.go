package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaSQL creates the enrolment tables.
//
//go:embed schema.sql
var schemaSQL string

// uniqueViolation is the SQLSTATE Postgres raises for a unique constraint.
const uniqueViolation = "23505"

// registrationKeyIndex is the index that makes one registration key enrol at
// most one agent. A violation of this index specifically means a retry, not a
// collision of ids.
const registrationKeyIndex = "agents_registration_key"

// Migrate creates the enrolment tables if they do not exist. Like the restore
// schema it does not alter existing tables: when the schema changes during
// development the database is dropped and recreated.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply agent schema: %w", err)
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

// Durations are passed as milliseconds and turned into an interval in SQL, so
// the conversion does not depend on how the driver encodes time.Duration.

const insertTokenSQL = `
INSERT INTO registration_tokens (token_id, token_hash, expires_at, max_uses, note)
VALUES ($1, $2, now() + ($3::bigint * interval '1 millisecond'), $4, $5)
RETURNING expires_at`

// InsertRegistrationToken implements Store.
func (s *PGStore) InsertRegistrationToken(ctx context.Context, tokenID string, hash []byte, ttl time.Duration, maxUses int, note string) (time.Time, error) {
	var expiresAt time.Time
	err := s.pool.QueryRow(ctx, insertTokenSQL, tokenID, hash, ttl.Milliseconds(), maxUses, note).Scan(&expiresAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("insert registration token: %w", err)
	}
	return expiresAt, nil
}

// registerSQL spends the token and creates the agent in one statement.
//
// The CTE's UPDATE only matches a token that is live and has a use left, and
// the INSERT draws its rows from that UPDATE — so an agent cannot be created
// without spending a use, and because a statement is atomic, a use cannot be
// spent without creating the agent. A duplicate registration key fails the
// unique index and rolls the whole statement back, the increment included.
//
// ON CONFLICT DO NOTHING must never be added here: it would skip the INSERT
// while still committing the CTE's increment, burning a use and enrolling
// nobody.
const registerSQL = `
WITH consumed AS (
    UPDATE registration_tokens
       SET uses = uses + 1
     WHERE token_hash = $1
       AND revoked_at IS NULL
       AND expires_at > now()
       AND uses < max_uses
    RETURNING token_id
)
INSERT INTO agents (agent_id, system_id, token_hash, hostname, agent_version,
                    mode, registration_token_id, registration_key)
SELECT $2, $3, $4, $5, $6, $7, token_id, $8 FROM consumed
RETURNING agent_id, system_id, hostname, agent_version, mode,
          registration_token_id, registration_key, registered_at`

// registeredByKeySQL finds the agent a registration key already produced. It
// answers the question the zero-row case cannot: was the token merely used up,
// or did this very caller already succeed?
const registeredByKeySQL = `
SELECT a.agent_id, a.system_id, a.hostname, a.agent_version, a.mode,
       a.registration_token_id, a.registration_key, a.registered_at
  FROM agents a
  JOIN registration_tokens t ON t.token_id = a.registration_token_id
 WHERE t.token_hash = $1 AND a.registration_key = $2`

// Register implements Store.
func (s *PGStore) Register(ctx context.Context, tokenHash []byte, a NewAgent) (Agent, error) {
	row := s.pool.QueryRow(ctx, registerSQL,
		tokenHash, a.AgentID, a.SystemID, a.TokenHash, a.Hostname, a.AgentVersion,
		string(a.Mode), nullableKey(a.RegistrationKey),
	)

	registered, err := scanAgent(row)
	switch {
	case err == nil:
		return registered, nil

	case errors.Is(err, pgx.ErrNoRows):
		// The token could not be spent. Either it is unusable, or this key
		// already enrolled an agent and the caller is retrying.
		if a.RegistrationKey != "" {
			if existing, lookupErr := s.registeredByKey(ctx, tokenHash, a.RegistrationKey); lookupErr == nil {
				return existing, ErrAlreadyRegistered
			} else if !errors.Is(lookupErr, pgx.ErrNoRows) {
				return Agent{}, lookupErr
			}
		}
		return Agent{}, ErrTokenRejected

	case isUniqueViolation(err, registrationKeyIndex):
		// The token had a use left, so the UPDATE matched and the INSERT then
		// hit the key. The statement rolled back, so no use was spent.
		if existing, lookupErr := s.registeredByKey(ctx, tokenHash, a.RegistrationKey); lookupErr == nil {
			return existing, ErrAlreadyRegistered
		}
		return Agent{}, ErrAlreadyRegistered

	default:
		return Agent{}, fmt.Errorf("register agent: %w", err)
	}
}

func (s *PGStore) registeredByKey(ctx context.Context, tokenHash []byte, key string) (Agent, error) {
	return scanAgent(s.pool.QueryRow(ctx, registeredByKeySQL, tokenHash, key))
}

const authenticateSQL = `
SELECT agent_id, system_id, hostname, agent_version, mode,
       registration_token_id, registration_key, registered_at, revoked_at
  FROM agents WHERE token_hash = $1`

// AuthenticateAgent implements Store.
func (s *PGStore) AuthenticateAgent(ctx context.Context, tokenHash []byte) (Agent, error) {
	var (
		a         Agent
		mode      string
		hostname  *string
		version   *string
		tokenID   *string
		key       *string
		revokedAt *time.Time
	)
	err := s.pool.QueryRow(ctx, authenticateSQL, tokenHash).Scan(
		&a.AgentID, &a.SystemID, &hostname, &version, &mode,
		&tokenID, &key, &a.RegisteredAt, &revokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, ErrAgentNotFound
	}
	if err != nil {
		return Agent{}, fmt.Errorf("authenticate agent: %w", err)
	}

	a.Mode = Mode(mode)
	a.Hostname = derefString(hostname)
	a.AgentVersion = derefString(version)
	a.RegistrationTokenID = derefString(tokenID)
	a.RegistrationKey = derefString(key)
	a.RevokedAt = revokedAt

	if revokedAt != nil {
		return a, ErrAgentRevoked
	}
	return a, nil
}

// TouchAgent implements Store.
func (s *PGStore) TouchAgent(ctx context.Context, agentID string) error {
	const q = `UPDATE agents SET last_seen_at = now() WHERE agent_id = $1`
	if _, err := s.pool.Exec(ctx, q, agentID); err != nil {
		return fmt.Errorf("record agent contact: %w", err)
	}
	return nil
}

const listAgentsSQL = `
SELECT agent_id, system_id, hostname, agent_version, mode,
       registration_token_id, registration_key, registered_at, last_seen_at,
       revoked_at, now()
  FROM agents ORDER BY registered_at DESC`

// ListAgents implements Store.
func (s *PGStore) ListAgents(ctx context.Context) ([]Agent, time.Time, error) {
	rows, err := s.pool.Query(ctx, listAgentsSQL)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("list agents: %w", err)
	}
	defer rows.Close()

	var (
		out []Agent
		now time.Time
	)
	for rows.Next() {
		var (
			a        Agent
			mode     string
			hostname *string
			version  *string
			tokenID  *string
			key      *string
		)
		if err := rows.Scan(&a.AgentID, &a.SystemID, &hostname, &version, &mode,
			&tokenID, &key, &a.RegisteredAt, &a.LastSeenAt, &a.RevokedAt, &now); err != nil {
			return nil, time.Time{}, fmt.Errorf("scan agent: %w", err)
		}
		a.Mode = Mode(mode)
		a.Hostname = derefString(hostname)
		a.AgentVersion = derefString(version)
		a.RegistrationTokenID = derefString(tokenID)
		a.RegistrationKey = derefString(key)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("list agents: %w", err)
	}
	if now.IsZero() {
		// No rows, so no clock came back with them; ask for it.
		if err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return nil, time.Time{}, fmt.Errorf("read store clock: %w", err)
		}
	}
	return out, now, nil
}

// RevokeAgent implements Store.
func (s *PGStore) RevokeAgent(ctx context.Context, agentID string) error {
	const q = `UPDATE agents SET revoked_at = now()
	            WHERE agent_id = $1 AND revoked_at IS NULL`
	if _, err := s.pool.Exec(ctx, q, agentID); err != nil {
		return fmt.Errorf("revoke agent: %w", err)
	}
	return nil
}

const listTokensSQL = `
SELECT token_id, expires_at, max_uses, uses, note, created_at, revoked_at, now()
  FROM registration_tokens ORDER BY created_at DESC`

// ListRegistrationTokens implements Store.
func (s *PGStore) ListRegistrationTokens(ctx context.Context) ([]RegistrationToken, time.Time, error) {
	rows, err := s.pool.Query(ctx, listTokensSQL)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("list registration tokens: %w", err)
	}
	defer rows.Close()

	var (
		out []RegistrationToken
		now time.Time
	)
	for rows.Next() {
		var (
			t    RegistrationToken
			note *string
		)
		if err := rows.Scan(&t.TokenID, &t.ExpiresAt, &t.MaxUses, &t.Uses,
			&note, &t.CreatedAt, &t.RevokedAt, &now); err != nil {
			return nil, time.Time{}, fmt.Errorf("scan registration token: %w", err)
		}
		t.Note = derefString(note)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("list registration tokens: %w", err)
	}
	if now.IsZero() {
		if err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return nil, time.Time{}, fmt.Errorf("read store clock: %w", err)
		}
	}
	return out, now, nil
}

// RevokeRegistrationToken implements Store.
func (s *PGStore) RevokeRegistrationToken(ctx context.Context, tokenID string) error {
	const q = `UPDATE registration_tokens SET revoked_at = now()
	            WHERE token_id = $1 AND revoked_at IS NULL`
	if _, err := s.pool.Exec(ctx, q, tokenID); err != nil {
		return fmt.Errorf("revoke registration token: %w", err)
	}
	return nil
}

// scanner abstracts pgx.Row for shared scanning.
type scanner interface {
	Scan(dest ...any) error
}

// scanAgent reads the columns registerSQL and registeredByKeySQL return.
func scanAgent(sc scanner) (Agent, error) {
	var (
		a        Agent
		mode     string
		hostname *string
		version  *string
		tokenID  *string
		key      *string
	)
	if err := sc.Scan(&a.AgentID, &a.SystemID, &hostname, &version, &mode,
		&tokenID, &key, &a.RegisteredAt); err != nil {
		return Agent{}, err
	}
	a.Mode = Mode(mode)
	a.Hostname = derefString(hostname)
	a.AgentVersion = derefString(version)
	a.RegistrationTokenID = derefString(tokenID)
	a.RegistrationKey = derefString(key)
	return a, nil
}

// isUniqueViolation reports whether err is a unique violation on the named
// constraint.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == uniqueViolation &&
		pgErr.ConstraintName == constraint
}

// nullableKey maps an empty registration key to NULL, so that agents enrolled
// without one do not collide in the partial unique index.
func nullableKey(key string) *string {
	if key == "" {
		return nil
	}
	return &key
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
