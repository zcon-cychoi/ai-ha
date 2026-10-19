package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/framework/token"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests exercise the SQL against a real PostgreSQL. They run only when
// AIHA_TEST_DB_DSN points at a disposable database.
//
// The fake store in service_test.go cannot stand in for them: the properties
// that matter here — that one statement spends a use and creates an agent
// together, and that a duplicate key rolls the increment back — are properties
// of the statement, not of the interface.

func testPGService(t *testing.T) (*Service, *PGStore, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("AIHA_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("AIHA_TEST_DB_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	store := NewPGStore(pool)
	return NewService(store), store, pool
}

// issuePG creates a token and schedules the rows it produces for cleanup.
func issuePG(t *testing.T, svc *Service, pool *pgxpool.Pool, ttl time.Duration, maxUses int) IssuedToken {
	t.Helper()
	issued, err := svc.IssueRegistrationToken(context.Background(), ttl, maxUses, "pg test")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM agents WHERE registration_token_id = $1`, issued.TokenID)
		_, _ = pool.Exec(ctx, `DELETE FROM registration_tokens WHERE token_id = $1`, issued.TokenID)
	})
	return issued
}

func usesOfPG(t *testing.T, pool *pgxpool.Pool, tokenID string) int {
	t.Helper()
	var uses int
	err := pool.QueryRow(context.Background(),
		`SELECT uses FROM registration_tokens WHERE token_id = $1`, tokenID).Scan(&uses)
	if err != nil {
		t.Fatalf("read uses: %v", err)
	}
	return uses
}

func TestPGRegisterRoundTrip(t *testing.T) {
	svc, _, pool := testPGService(t)
	issued := issuePG(t, svc, pool, time.Minute, 1)

	got, err := svc.Register(context.Background(), issued.Token, registration(testKey))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	authed, err := svc.Authenticate(context.Background(), got.Token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if authed.AgentID != got.AgentID || authed.SystemID != "sys-0123456789abcdef" {
		t.Fatalf("authenticated as %+v", authed)
	}
	if authed.Mode != ModeNormal {
		t.Fatalf("mode = %q", authed.Mode)
	}
	if uses := usesOfPG(t, pool, issued.TokenID); uses != 1 {
		t.Fatalf("token spent %d uses, want 1", uses)
	}
}

// The statement must not let two agents share one use, nor let a use be spent
// without producing an agent.
func TestPGRegisterIsAtomicUnderConcurrency(t *testing.T) {
	svc, _, pool := testPGService(t)
	issued := issuePG(t, svc, pool, time.Minute, 1)

	const n = 20
	results := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, results[i] = svc.Register(context.Background(), issued.Token,
				registration(fmt.Sprintf("%032x", i)))
		})
	}
	wg.Wait()

	var succeeded int
	for i, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrTokenRejected):
		default:
			t.Fatalf("goroutine %d: unexpected error %v", i, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d enrolments succeeded on a single-use token, want 1", succeeded)
	}
	if uses := usesOfPG(t, pool, issued.TokenID); uses != 1 {
		t.Fatalf("token spent %d uses, want 1", uses)
	}

	var agents int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agents WHERE registration_token_id = $1`, issued.TokenID).Scan(&agents); err != nil {
		t.Fatalf("count agents: %v", err)
	}
	if agents != 1 {
		t.Fatalf("%d agents created, want 1", agents)
	}
}

// A retried enrolment must be recognised, and must not cost a use. This is the
// test that catches an ON CONFLICT DO NOTHING creeping into registerSQL: that
// would skip the INSERT while committing the CTE's increment.
func TestPGDuplicateKeyDoesNotSpendAUse(t *testing.T) {
	for _, maxUses := range []int{1, 5} {
		name := "uses remaining"
		if maxUses == 1 {
			name = "single-use"
		}
		t.Run(name, func(t *testing.T) {
			svc, _, pool := testPGService(t)
			issued := issuePG(t, svc, pool, time.Minute, maxUses)

			first, err := svc.Register(context.Background(), issued.Token, registration(testKey))
			if err != nil {
				t.Fatalf("first register: %v", err)
			}
			usesAfterFirst := usesOfPG(t, pool, issued.TokenID)

			_, err = svc.Register(context.Background(), issued.Token, registration(testKey))
			if !errors.Is(err, ErrAlreadyRegistered) {
				t.Fatalf("err = %v, want ErrAlreadyRegistered", err)
			}
			if got := usesOfPG(t, pool, issued.TokenID); got != usesAfterFirst {
				t.Fatalf("a refused retry spent a use: %d -> %d", usesAfterFirst, got)
			}

			var agents int
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM agents WHERE registration_token_id = $1`, issued.TokenID).Scan(&agents); err != nil {
				t.Fatalf("count agents: %v", err)
			}
			if agents != 1 {
				t.Fatalf("%d agents exist after a retry, want 1 (%s)", agents, first.AgentID)
			}
		})
	}
}

func TestPGRejectsUnusableTokens(t *testing.T) {
	svc, _, pool := testPGService(t)

	t.Run("expired", func(t *testing.T) {
		// The service refuses to issue an already-expired token, so the row is
		// aged directly. The expiry is evaluated on the store's clock, which is
		// the point of the test.
		issued := issuePG(t, svc, pool, time.Minute, 1)
		if _, err := pool.Exec(context.Background(),
			`UPDATE registration_tokens SET expires_at = now() - interval '1 second'
			  WHERE token_id = $1`, issued.TokenID); err != nil {
			t.Fatalf("age the token: %v", err)
		}

		_, err := svc.Register(context.Background(), issued.Token, registration(testKey))
		if !errors.Is(err, ErrTokenRejected) {
			t.Fatalf("err = %v, want ErrTokenRejected", err)
		}
	})

	t.Run("revoked", func(t *testing.T) {
		issued := issuePG(t, svc, pool, time.Minute, 5)
		if err := svc.RevokeRegistrationToken(context.Background(), issued.TokenID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		_, err := svc.Register(context.Background(), issued.Token, registration(testKey))
		if !errors.Is(err, ErrTokenRejected) {
			t.Fatalf("err = %v, want ErrTokenRejected", err)
		}
	})

	t.Run("used up", func(t *testing.T) {
		issued := issuePG(t, svc, pool, time.Minute, 1)
		if _, err := svc.Register(context.Background(), issued.Token, registration(testKey)); err != nil {
			t.Fatalf("first register: %v", err)
		}
		_, err := svc.Register(context.Background(), issued.Token,
			registration(strings.Repeat("a", 32)))
		if !errors.Is(err, ErrTokenRejected) {
			t.Fatalf("err = %v, want ErrTokenRejected", err)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		_, err := svc.Register(context.Background(), "not-a-real-token", registration(testKey))
		if !errors.Is(err, ErrTokenRejected) {
			t.Fatalf("err = %v, want ErrTokenRejected", err)
		}
	})
}

func TestPGRevokedAgentIsRejected(t *testing.T) {
	svc, _, pool := testPGService(t)
	issued := issuePG(t, svc, pool, time.Minute, 1)

	got, err := svc.Register(context.Background(), issued.Token, registration(testKey))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := svc.RevokeAgent(context.Background(), got.AgentID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, err := svc.Authenticate(context.Background(), got.Token); !errors.Is(err, ErrAgentRevoked) {
		t.Fatalf("err = %v, want ErrAgentRevoked", err)
	}
}

func TestPGTouchAndOnline(t *testing.T) {
	svc, store, pool := testPGService(t)
	issued := issuePG(t, svc, pool, time.Minute, 1)

	got, err := svc.Register(context.Background(), issued.Token, registration(testKey))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := svc.Touch(context.Background(), got.AgentID); err != nil {
		t.Fatalf("touch: %v", err)
	}

	agents, now, err := store.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if now.IsZero() {
		t.Fatal("the store clock did not come back with the rows")
	}
	for _, a := range agents {
		if a.AgentID != got.AgentID {
			continue
		}
		if a.LastSeenAt == nil {
			t.Fatal("last contact was not recorded")
		}
		if !isOnline(a, now) {
			t.Fatal("an agent that just called is not online")
		}
		return
	}
	t.Fatalf("agent %s missing from the listing", got.AgentID)
}

// An unknown token must be a miss, not an error, so that the middleware can
// tell a rejected caller from a broken database.
func TestPGAuthenticateUnknownToken(t *testing.T) {
	_, store, _ := testPGService(t)

	_, err := store.AuthenticateAgent(context.Background(), token.Hash("nobody"))
	if !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("err = %v, want ErrAgentNotFound", err)
	}
}
