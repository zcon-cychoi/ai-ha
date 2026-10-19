package rescue

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest"
	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/cloud-barista/ai-ha/pkg/framework/restore"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRunAgainstFrameworkAfterLostResponse drives the agent against the
// framework's real handler and PostgreSQL store. The first claim is committed
// and then its connection is dropped, as when a response is lost in transit;
// the agent must recover it by retrying under the same request key. It runs
// only when AIHA_TEST_DB_DSN points at a disposable database.
func TestRunAgainstFrameworkAfterLostResponse(t *testing.T) {
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
	if err := restore.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	svc := restore.NewService(restore.NewPGStore(pool), oneSet{}, oneRepository{}, time.Minute, time.Minute)
	created, err := svc.Create(ctx, restore.NewJob{BackupSetID: "9c0d1e2f3a4b5c6d"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM restore_jobs WHERE restore_id = $1`, created.RestoreID)
	})

	handler := rest.NewHandler(rest.Config{Restore: svc})
	var dropped atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dropped.CompareAndSwap(false, true) {
			// Let the framework commit the claim, then lose its response.
			committed := httptest.NewRecorder()
			handler.ServeHTTP(committed, r)
			if committed.Code != http.StatusOK {
				t.Errorf("first claim: status %d (%s)", committed.Code, committed.Body)
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	pin := sha256.Sum256(srv.Certificate().Raw)

	opts := testOptions(t, writeRecoveryFor(t, created.RecoveryID, created.ClaimToken, srv.URL, pin[:]))
	res, err := Run(ctx, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.RestoreID != created.RestoreID {
		t.Fatalf("staged restore %s, want %s", res.RestoreID, created.RestoreID)
	}
	if !dropped.Load() {
		t.Fatal("the first response was never dropped")
	}
	// The job was claimed once, under the agent's key: another key conflicts.
	if _, err := svc.Claim(ctx, created.RecoveryID, created.ClaimToken, "other-key"); !errors.Is(err, restore.ErrClaimedByOther) {
		t.Fatalf("claim with another key: err = %v, want ErrClaimedByOther", err)
	}

	// The report token staged from the redelivered response is the one the
	// framework accepts.
	staged, err := LoadStaged(res.Dir)
	if err != nil {
		t.Fatalf("load staged: %v", err)
	}
	rec, err := LoadRecovery(opts.RecoveryFile)
	if err != nil {
		t.Fatalf("load recovery: %v", err)
	}
	reports := NewReportClient(rec, staged.ReportToken)
	if err := reports.Progress(ctx, model.RestoreProgressReq{Seq: 1, Phase: "extracting", Step: 4, TotalSteps: 6, ProcessedBytes: 5, TotalBytes: 10}); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := reports.Result(ctx, model.RestoreResultReq{Outcome: "succeeded", Phase: "bootloader"}); err != nil {
		t.Fatalf("result: %v", err)
	}
	job, err := svc.Get(ctx, created.RestoreID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Status != restore.StatusAwaitingBoot || job.Progress.ProcessedBytes != 5 || job.Outcome != restore.OutcomeSucceeded {
		t.Fatalf("job after reports: status %s, progress %+v, outcome %s", job.Status, job.Progress, job.Outcome)
	}
}

// oneSet and oneRepository stand in for the backup job records and the
// repository registry, which have their own tests.
type oneSet struct{}

func (oneSet) FindBackupSet(context.Context, string) (job.BackupSetOrigin, error) {
	return job.BackupSetOrigin{JobID: "job-1", SystemID: "web01-3f2a9c1e", RepositoryID: "0123456789abcdef"}, nil
}

type oneRepository struct{}

func (oneRepository) Access(context.Context, string) (model.RepositoryAccess, error) {
	return model.RepositoryAccess{
		Type: "s3", Provider: "Minio", Endpoint: "https://storage.example.com:9000",
		Bucket: "aiha-backup", AccessKey: "test-access", SecretKey: testSecret,
	}, nil
}
