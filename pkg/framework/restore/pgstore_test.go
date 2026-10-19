package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests exercise the SQL against a real PostgreSQL. They run only when
// AIHA_TEST_DB_DSN points at a disposable database.

func testPGService(t *testing.T, claimTTL, redelivery time.Duration) (*Service, *pgxpool.Pool) {
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
	return newTestService(NewPGStore(pool), claimTTL, redelivery), pool
}

func createPGJob(t *testing.T, s *Service, pool *pgxpool.Pool) Created {
	t.Helper()
	created, err := s.Create(context.Background(), validJob())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM restore_jobs WHERE restore_id = $1`, created.RestoreID)
	})
	return created
}

// sameJSON reports whether a and b encode the same JSON value. jsonb
// normalizes whitespace and key order, so bytes are not compared.
func sameJSON(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return reflect.DeepEqual(va, vb)
}

func TestPGClaimRoundTrip(t *testing.T) {
	s, pool := testPGService(t, testTTL, testRedelivery)
	created := createPGJob(t, s, pool)

	resp, err := s.Claim(context.Background(), created.RecoveryID, created.ClaimToken, "key-a")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	want := json.RawMessage(`{"sourceSystemId":"` + testSystemID + `","sourceBackupSetId":"` + testSetID + `"}`)
	if !sameJSON(t, resp.Plan, want) {
		t.Fatalf("plan changed in storage: %s", resp.Plan)
	}
	if resp.Repository.SecretKey != "test-secret" || resp.Repository.Bucket != "aiha-backup" {
		t.Fatalf("claim carries repository %+v", resp.Repository)
	}

	// The restore job keeps the repository's ID, never its credentials.
	var repoID string
	if err := pool.QueryRow(context.Background(),
		`SELECT repository_id FROM restore_jobs WHERE restore_id = $1`, created.RestoreID).Scan(&repoID); err != nil || repoID != testRepoID {
		t.Fatalf("repository_id = %q, %v", repoID, err)
	}
	var plaintext bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'restore_jobs' AND column_name = 'repository')`).Scan(&plaintext); err != nil || plaintext {
		t.Fatalf("restore_jobs still has the plaintext repository column (%v)", err)
	}
}

func TestPGConcurrentClaimsWithDifferentKeysHaveOneWinner(t *testing.T) {
	s, pool := testPGService(t, testTTL, testRedelivery)
	created := createPGJob(t, s, pool)

	const n = 20
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, errs[i] = s.Claim(context.Background(), created.RecoveryID, created.ClaimToken, fmt.Sprintf("key-%d", i))
		})
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrClaimedByOther):
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d claims succeeded, want exactly 1", wins)
	}
}

func TestPGConcurrentClaimsWithSameKeyAllSucceed(t *testing.T) {
	s, pool := testPGService(t, testTTL, testRedelivery)
	created := createPGJob(t, s, pool)

	const n = 20
	type result struct {
		plan json.RawMessage
		err  error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			resp, err := s.Claim(context.Background(), created.RecoveryID, created.ClaimToken, "key-a")
			results[i] = result{resp.Plan, err}
		})
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("claim %d: %v", i, r.err)
		}
		if !sameJSON(t, r.plan, results[0].plan) {
			t.Fatalf("claim %d returned different content", i)
		}
	}
}

func TestPGClaimWindowExpires(t *testing.T) {
	s, pool := testPGService(t, 50*time.Millisecond, testRedelivery)
	created := createPGJob(t, s, pool)

	time.Sleep(200 * time.Millisecond)
	if _, err := s.Claim(context.Background(), created.RecoveryID, created.ClaimToken, "key-a"); !errors.Is(err, ErrClaimExpired) {
		t.Fatalf("err = %v, want ErrClaimExpired", err)
	}
}

func TestPGRedeliveryWindowExpires(t *testing.T) {
	s, pool := testPGService(t, testTTL, 50*time.Millisecond)
	created := createPGJob(t, s, pool)
	ctx := context.Background()

	if _, err := s.Claim(ctx, created.RecoveryID, created.ClaimToken, "key-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := s.Claim(ctx, created.RecoveryID, created.ClaimToken, "key-a"); !errors.Is(err, ErrRedeliveryExpired) {
		t.Fatalf("err = %v, want ErrRedeliveryExpired", err)
	}
}

func TestPGMigrateIsRepeatable(t *testing.T) {
	_, pool := testPGService(t, testTTL, testRedelivery)
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestPGReportsRoundTrip(t *testing.T) {
	s, pool := testPGService(t, testTTL, testRedelivery)
	created := createPGJob(t, s, pool)
	ctx := context.Background()

	resp, err := s.Claim(ctx, created.RecoveryID, created.ClaimToken, "key-a")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	tok := resp.ReportToken

	p := Progress{Seq: 2, Phase: "extracting", Step: 4, TotalSteps: 6, Item: "a.aha", ProcessedBytes: 10, TotalBytes: 100}
	if err := s.RecordProgress(ctx, created.RecoveryID, tok, p); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := s.RecordProgress(ctx, created.RecoveryID, tok, Progress{Seq: 1, Phase: "preparing"}); err != nil {
		t.Fatalf("stale progress: %v", err)
	}
	job, err := s.Get(ctx, created.RestoreID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	byRecovery, err := s.Get(ctx, created.RecoveryID)
	if err != nil || byRecovery.RestoreID != created.RestoreID {
		t.Fatalf("get by recovery ID: %s, %v", byRecovery.RestoreID, err)
	}
	if _, err := s.Get(ctx, "0000000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ID: err = %v, want ErrNotFound", err)
	}
	if job.Status != StatusRestoring || job.Progress != p || job.LastProgressAt == nil {
		t.Fatalf("after progress: status %s, progress %+v, last %v", job.Status, job.Progress, job.LastProgressAt)
	}

	r := Result{Outcome: OutcomeSucceeded, Phase: "bootloader", Message: "Restored"}
	for i := range 2 {
		status, err := s.RecordResult(ctx, created.RecoveryID, tok, r)
		if err != nil || status != StatusAwaitingBoot {
			t.Fatalf("result #%d: status %s, err %v", i+1, status, err)
		}
	}
	if _, err := s.RecordResult(ctx, created.RecoveryID, tok, Result{Outcome: OutcomeFailed}); !errors.Is(err, ErrResultConflict) {
		t.Fatalf("different result: err = %v, want ErrResultConflict", err)
	}
	job, err = s.Get(ctx, created.RestoreID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Outcome != OutcomeSucceeded || job.Message != "Restored" || job.FinishedAt == nil || job.Progress.Phase != "bootloader" {
		t.Fatalf("after result: %+v", job)
	}
}

func TestPGPreparingFailureReopens(t *testing.T) {
	s, pool := testPGService(t, testTTL, testRedelivery)
	created := createPGJob(t, s, pool)
	ctx := context.Background()
	resp, err := s.Claim(ctx, created.RecoveryID, created.ClaimToken, "key-a")
	if err != nil {
		t.Fatal(err)
	}
	tok := resp.ReportToken
	if err := s.RecordProgress(ctx, created.RecoveryID, tok, Progress{Seq: 5, Phase: "preparing", Step: 1, TotalSteps: 6}); err != nil {
		t.Fatal(err)
	}
	if status, err := s.RecordResult(ctx, created.RecoveryID, tok,
		Result{Outcome: OutcomeFailed, Phase: "preparing", Message: "tool missing"}); err != nil || status != StatusClaimed {
		t.Fatalf("status %s, err %v", status, err)
	}
	// The second run numbers its reports from 1; they must count.
	if err := s.RecordProgress(ctx, created.RecoveryID, tok, Progress{Seq: 1, Phase: "formatting", Step: 2, TotalSteps: 6}); err != nil {
		t.Fatal(err)
	}
	job, err := s.Get(ctx, created.RestoreID)
	if err != nil || job.Status != StatusRestoring || job.Progress.Phase != "formatting" || job.Outcome != "" {
		t.Fatalf("job = %+v, %v", job, err)
	}
}
