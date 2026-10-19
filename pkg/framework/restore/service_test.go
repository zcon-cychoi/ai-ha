package restore

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/cloud-barista/ai-ha/pkg/framework/token"
)

// fakeStore is an in-memory Store with a controllable clock. TryClaim is atomic
// under mu, like the conditional UPDATE in PGStore.
type fakeStore struct {
	mu   sync.Mutex
	now  time.Time
	jobs map[string]Job
	// beforeTryClaim, when set, runs just before TryClaim to simulate a request
	// that lands between Claim's read and its update.
	beforeTryClaim func()
}

func newFakeStore() *fakeStore {
	return &fakeStore{now: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), jobs: map[string]Job{}}
}

func (f *fakeStore) Insert(_ context.Context, job Job, ttl time.Duration) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job.ClaimExpiresAt = f.now.Add(ttl)
	f.jobs[job.RecoveryID] = job
	return job.ClaimExpiresAt, nil
}

func (f *fakeStore) Get(_ context.Context, recoveryID string) (Job, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[recoveryID]
	if !ok {
		return Job{}, time.Time{}, ErrNotFound
	}
	return job, f.now, nil
}

func (f *fakeStore) TryClaim(_ context.Context, recoveryID, requestKey string, redelivery time.Duration) (bool, error) {
	if f.beforeTryClaim != nil {
		f.beforeTryClaim()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claimLocked(recoveryID, requestKey, redelivery), nil
}

func (f *fakeStore) claimLocked(recoveryID, requestKey string, redelivery time.Duration) bool {
	job, ok := f.jobs[recoveryID]
	if !ok || job.Status != StatusAwaitingAgent || job.ClaimRequestKey != "" || !f.now.Before(job.ClaimExpiresAt) {
		return false
	}
	claimedAt, until := f.now, f.now.Add(redelivery)
	job.Status, job.ClaimRequestKey = StatusClaimed, requestKey
	job.ClaimedAt, job.RedeliveryExpiresAt = &claimedAt, &until
	f.jobs[recoveryID] = job
	return true
}

func (f *fakeStore) SetReportToken(_ context.Context, recoveryID, requestKey string, hash []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[recoveryID]
	if !ok || job.ClaimRequestKey != requestKey {
		return ErrNotFound
	}
	job.ReportTokenHash = hash
	f.jobs[recoveryID] = job
	return nil
}

func (f *fakeStore) UpdateProgress(_ context.Context, recoveryID string, p Progress) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[recoveryID]
	if !ok || !job.Status.active() || p.Seq <= job.Progress.Seq {
		return false, nil
	}
	now := f.now
	job.Status, job.Progress, job.LastProgressAt = StatusRestoring, p, &now
	f.jobs[recoveryID] = job
	return true, nil
}

func (f *fakeStore) Finish(_ context.Context, recoveryID string, status Status, r Result) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[recoveryID]
	if !ok || !job.Status.active() {
		return false, nil
	}
	now := f.now
	job.Status, job.Outcome, job.Message, job.FinishedAt = status, r.Outcome, r.Message, &now
	f.jobs[recoveryID] = job
	return true, nil
}

func (f *fakeStore) Reopen(_ context.Context, recoveryID string, r Result) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[recoveryID]
	if !ok || !job.Status.active() {
		return false, nil
	}
	job.Status, job.Message = StatusClaimed, r.Message
	job.Progress = Progress{Phase: r.Phase}
	f.jobs[recoveryID] = job
	return true, nil
}

func (f *fakeStore) Lookup(_ context.Context, id string) (Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, job := range f.jobs {
		if job.RestoreID == id || job.RecoveryID == id {
			return job, nil
		}
	}
	return Job{}, ErrNotFound
}

func (f *fakeStore) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

const (
	testTTL        = 30 * time.Minute
	testRedelivery = 10 * time.Minute
)

const (
	testSetID    = "9c0d1e2f3a4b5c6d"
	testSystemID = "web01-3f2a9c1e"
	testRepoID   = "0123456789abcdef"
)

// fakeSets knows one completed backup set.
type fakeSets struct{}

func (fakeSets) FindBackupSet(_ context.Context, setID string) (job.BackupSetOrigin, error) {
	if setID == testSetID {
		return job.BackupSetOrigin{JobID: "job-1", SystemID: testSystemID, RepositoryID: testRepoID}, nil
	}
	return job.BackupSetOrigin{}, job.ErrBackupSetNotFound
}

// fakeRepos opens the test repository unless told to fail, as an
// undecryptable row or a missing key would.
type fakeRepos struct {
	mu   sync.Mutex
	fail bool
}

func (f *fakeRepos) Access(_ context.Context, id string) (model.RepositoryAccess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail || id != testRepoID {
		return model.RepositoryAccess{}, errors.New("secret cannot be decrypted with this key")
	}
	return model.RepositoryAccess{
		Type: "s3", Provider: "Minio", Endpoint: "https://storage.example.com:9000",
		Bucket: "aiha-backup", AccessKey: "test-access", SecretKey: "test-secret",
	}, nil
}

func (f *fakeRepos) setFail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = v
}

func newTestService(store Store, claimTTL, redelivery time.Duration) *Service {
	return NewService(store, fakeSets{}, &fakeRepos{}, claimTTL, redelivery)
}

func validJob() NewJob {
	return NewJob{BackupSetID: testSetID}
}

func createJob(t *testing.T, s *Service) Created {
	t.Helper()
	created, err := s.Create(context.Background(), validJob())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return created
}

func TestCreateStoresOnlyTokenHash(t *testing.T) {
	store := newFakeStore()
	created := createJob(t, newTestService(store, testTTL, testRedelivery))

	job := store.jobs[created.RecoveryID]
	if string(job.ClaimTokenHash) == created.ClaimToken {
		t.Fatal("store holds the plaintext claim token")
	}
	if !token.Matches(job.ClaimTokenHash, created.ClaimToken) {
		t.Fatal("stored hash does not match the issued token")
	}
	if job.Status != StatusAwaitingAgent {
		t.Fatalf("status = %s, want %s", job.Status, StatusAwaitingAgent)
	}
	if want := store.now.Add(testTTL); !created.ClaimExpiresAt.Equal(want) {
		t.Fatalf("claim window closes at %s, want %s", created.ClaimExpiresAt, want)
	}
}

func TestClaimFirstSucceedsAndMarksClaimed(t *testing.T) {
	store := newFakeStore()
	s := newTestService(store, testTTL, testRedelivery)
	created := createJob(t, s)

	resp, err := s.Claim(context.Background(), created.RecoveryID, created.ClaimToken, "key-a")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if resp.RecoveryID != created.RecoveryID || resp.RestoreID != created.RestoreID {
		t.Fatalf("response identifies the wrong job: %+v", resp)
	}
	if resp.Repository.Bucket != "aiha-backup" || resp.Repository.SecretKey != "test-secret" {
		t.Fatalf("repository access missing from response: %+v", resp.Repository)
	}
	if got := store.jobs[created.RecoveryID].Status; got != StatusClaimed {
		t.Fatalf("status = %s, want %s", got, StatusClaimed)
	}
}

func TestClaimSameKeyIsRedelivered(t *testing.T) {
	store := newFakeStore()
	s := newTestService(store, testTTL, testRedelivery)
	created := createJob(t, s)
	ctx := context.Background()

	// Claim late in the window so that the redelivery below lands after it closes.
	store.advance(testTTL - 5*time.Minute)
	first, err := s.Claim(ctx, created.RecoveryID, created.ClaimToken, "key-a")
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// The claim window has closed; the redelivery window is still open.
	store.advance(8 * time.Minute)
	again, err := s.Claim(ctx, created.RecoveryID, created.ClaimToken, "key-a")
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	// The report token is reissued; everything else is the same response.
	if first.ReportToken == "" || again.ReportToken == "" || first.ReportToken == again.ReportToken {
		t.Fatalf("report tokens: first %q, again %q; want two different tokens", first.ReportToken, again.ReportToken)
	}
	first.ReportToken, again.ReportToken = "", ""
	if !reflect.DeepEqual(first, again) {
		t.Fatalf("redelivered response differs:\nfirst %+v\nagain %+v", first, again)
	}
}

// claimJob creates and claims a job, returning it and the report token.
func claimJob(t *testing.T, s *Service) (Created, string) {
	t.Helper()
	created := createJob(t, s)
	resp, err := s.Claim(context.Background(), created.RecoveryID, created.ClaimToken, "key-a")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return created, resp.ReportToken
}

func TestReportTokenStoredAsHashAndOnlyLatestAccepted(t *testing.T) {
	store := newFakeStore()
	s := newTestService(store, testTTL, testRedelivery)
	created, first := claimJob(t, s)
	ctx := context.Background()

	if string(store.jobs[created.RecoveryID].ReportTokenHash) == first {
		t.Fatal("store holds the plaintext report token")
	}
	again, err := s.Claim(ctx, created.RecoveryID, created.ClaimToken, "key-a")
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}

	p := Progress{Seq: 1, Phase: "preparing", Step: 1, TotalSteps: 6}
	if err := s.RecordProgress(ctx, created.RecoveryID, first, p); !errors.Is(err, ErrReportTokenRejected) {
		t.Fatalf("superseded token: err = %v, want ErrReportTokenRejected", err)
	}
	if err := s.RecordProgress(ctx, created.RecoveryID, again.ReportToken, p); err != nil {
		t.Fatalf("latest token: %v", err)
	}
}

func TestReportsBeforeClaimAreRejected(t *testing.T) {
	s := newTestService(newFakeStore(), testTTL, testRedelivery)
	created := createJob(t, s)
	err := s.RecordProgress(context.Background(), created.RecoveryID, "anything", Progress{Seq: 1, Phase: "preparing"})
	if !errors.Is(err, ErrReportTokenRejected) {
		t.Fatalf("err = %v, want ErrReportTokenRejected", err)
	}
}

func TestProgressMovesToRestoringAndIgnoresStaleReports(t *testing.T) {
	store := newFakeStore()
	s := newTestService(store, testTTL, testRedelivery)
	created, tok := claimJob(t, s)
	ctx := context.Background()

	newer := Progress{Seq: 5, Phase: "extracting", Step: 4, TotalSteps: 6, Item: "a.aha", ProcessedBytes: 10, TotalBytes: 100}
	if err := s.RecordProgress(ctx, created.RecoveryID, tok, newer); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := s.RecordProgress(ctx, created.RecoveryID, tok, Progress{Seq: 3, Phase: "formatting"}); err != nil {
		t.Fatalf("stale progress: %v", err)
	}
	job := store.jobs[created.RecoveryID]
	if job.Status != StatusRestoring {
		t.Fatalf("status = %s, want %s", job.Status, StatusRestoring)
	}
	if job.Progress != newer {
		t.Fatalf("progress = %+v, want %+v", job.Progress, newer)
	}
}

func TestResultEndsJobAndIsIdempotent(t *testing.T) {
	tests := []struct {
		outcome Outcome
		want    Status
	}{
		{OutcomeSucceeded, StatusAwaitingBoot},
		{OutcomeFailed, StatusFailed},
	}
	for _, tt := range tests {
		t.Run(string(tt.outcome), func(t *testing.T) {
			s := newTestService(newFakeStore(), testTTL, testRedelivery)
			created, tok := claimJob(t, s)
			ctx := context.Background()
			r := Result{Outcome: tt.outcome, Phase: "bootloader", Message: "done"}

			for i := range 2 {
				status, err := s.RecordResult(ctx, created.RecoveryID, tok, r)
				if err != nil {
					t.Fatalf("result #%d: %v", i+1, err)
				}
				if status != tt.want {
					t.Fatalf("result #%d: status = %s, want %s", i+1, status, tt.want)
				}
			}

			other := r
			other.Message = "something else"
			if _, err := s.RecordResult(ctx, created.RecoveryID, tok, other); !errors.Is(err, ErrResultConflict) {
				t.Fatalf("different result: err = %v, want ErrResultConflict", err)
			}
			if err := s.RecordProgress(ctx, created.RecoveryID, tok, Progress{Seq: 9, Phase: "late"}); !errors.Is(err, ErrRestoreFinished) {
				t.Fatalf("progress after result: err = %v, want ErrRestoreFinished", err)
			}
		})
	}
}

func TestClaimErrors(t *testing.T) {
	tests := []struct {
		name string
		// run performs the scenario and returns Claim's error.
		run  func(s *Service, store *fakeStore, c Created) error
		want error
	}{
		{"unknown recovery", func(s *Service, _ *fakeStore, c Created) error {
			_, err := s.Claim(context.Background(), "no-such-id", c.ClaimToken, "key-a")
			return err
		}, ErrNotFound},
		{"wrong token", func(s *Service, _ *fakeStore, c Created) error {
			_, err := s.Claim(context.Background(), c.RecoveryID, "wrong", "key-a")
			return err
		}, ErrInvalidToken},
		{"other key after claim", func(s *Service, _ *fakeStore, c Created) error {
			if _, err := s.Claim(context.Background(), c.RecoveryID, c.ClaimToken, "key-a"); err != nil {
				return fmt.Errorf("first claim: %w", err)
			}
			_, err := s.Claim(context.Background(), c.RecoveryID, c.ClaimToken, "key-b")
			return err
		}, ErrClaimedByOther},
		{"claim window expired", func(s *Service, store *fakeStore, c Created) error {
			store.advance(testTTL)
			_, err := s.Claim(context.Background(), c.RecoveryID, c.ClaimToken, "key-a")
			return err
		}, ErrClaimExpired},
		{"redelivery window expired", func(s *Service, store *fakeStore, c Created) error {
			if _, err := s.Claim(context.Background(), c.RecoveryID, c.ClaimToken, "key-a"); err != nil {
				return fmt.Errorf("first claim: %w", err)
			}
			store.advance(testRedelivery)
			_, err := s.Claim(context.Background(), c.RecoveryID, c.ClaimToken, "key-a")
			return err
		}, ErrRedeliveryExpired},
		{"other key after redelivery window", func(s *Service, store *fakeStore, c Created) error {
			if _, err := s.Claim(context.Background(), c.RecoveryID, c.ClaimToken, "key-a"); err != nil {
				return fmt.Errorf("first claim: %w", err)
			}
			store.advance(testRedelivery)
			_, err := s.Claim(context.Background(), c.RecoveryID, c.ClaimToken, "key-b")
			return err
		}, ErrClaimedByOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			s := newTestService(store, testTTL, testRedelivery)
			if err := tt.run(s, store, createJob(t, s)); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestClaimWrongTokenLeavesJobUnclaimed(t *testing.T) {
	store := newFakeStore()
	s := newTestService(store, testTTL, testRedelivery)
	created := createJob(t, s)

	if _, err := s.Claim(context.Background(), created.RecoveryID, "wrong", "key-a"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
	if job := store.jobs[created.RecoveryID]; job.ClaimRequestKey != "" || job.Status != StatusAwaitingAgent {
		t.Fatalf("a rejected token changed the job: %+v", job)
	}
}

// TestClaimRaceBetweenReadAndUpdate covers a request that claims the job after
// Claim read it unclaimed but before its update: Claim must decide from the
// state re-read after the failed update.
func TestClaimRaceBetweenReadAndUpdate(t *testing.T) {
	tests := []struct {
		name    string
		raceKey string
		want    error
	}{
		{"other key wins", "key-b", ErrClaimedByOther},
		{"same key wins", "key-a", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			s := newTestService(store, testTTL, testRedelivery)
			created := createJob(t, s)
			store.beforeTryClaim = func() {
				store.mu.Lock()
				store.claimLocked(created.RecoveryID, tt.raceKey, testRedelivery)
				store.mu.Unlock()
			}

			_, err := s.Claim(context.Background(), created.RecoveryID, created.ClaimToken, "key-a")
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestConcurrentClaimsWithDifferentKeysHaveOneWinner(t *testing.T) {
	store := newFakeStore()
	s := newTestService(store, testTTL, testRedelivery)
	created := createJob(t, s)

	const n = 16
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

func TestCreateBuildsThePlanFromTheBackupSet(t *testing.T) {
	store := newFakeStore()
	created := createJob(t, newTestService(store, testTTL, testRedelivery))
	stored := store.jobs[created.RecoveryID]
	plan, err := model.DecodeRestorePlan(stored.Plan)
	if err != nil {
		t.Fatalf("stored plan: %v", err)
	}
	if plan.SourceSystemID != testSystemID || plan.SourceBackupSetID != testSetID || stored.RepositoryID != testRepoID {
		t.Fatalf("plan %+v, repository %q", plan, stored.RepositoryID)
	}
}

func TestCreateRejectsAnUnknownSet(t *testing.T) {
	s := newTestService(newFakeStore(), testTTL, testRedelivery)
	for _, id := range []string{"", "no-such-set"} {
		_, err := s.Create(context.Background(), NewJob{BackupSetID: id})
		if !errors.Is(err, ErrInvalidJob) || !errors.Is(err, job.ErrBackupSetNotFound) {
			t.Fatalf("set %q: err = %v, want ErrInvalidJob wrapping ErrBackupSetNotFound", id, err)
		}
	}
}

func TestClaimRefusesALegacyJob(t *testing.T) {
	store := newFakeStore()
	s := newTestService(store, testTTL, testRedelivery)
	created := createJob(t, s)
	legacy := store.jobs[created.RecoveryID]
	legacy.RepositoryID = ""
	store.jobs[created.RecoveryID] = legacy

	if _, err := s.Claim(context.Background(), created.RecoveryID, created.ClaimToken, "key-a"); !errors.Is(err, ErrLegacyJob) {
		t.Fatalf("err = %v, want ErrLegacyJob", err)
	}
	if store.jobs[created.RecoveryID].ClaimRequestKey != "" {
		t.Fatal("a refused claim was recorded")
	}
}

// The repository is opened before the claim is recorded, so a failure leaves
// the job as it was and the same request succeeds once it can be opened.
func TestClaimChangesNothingWhenTheRepositoryIsUnavailable(t *testing.T) {
	store := newFakeStore()
	repos := &fakeRepos{}
	s := NewService(store, fakeSets{}, repos, testTTL, testRedelivery)
	created := createJob(t, s)
	ctx := context.Background()

	repos.setFail(true)
	if _, err := s.Claim(ctx, created.RecoveryID, created.ClaimToken, "key-a"); err == nil {
		t.Fatal("claim succeeded without repository access")
	}
	if job := store.jobs[created.RecoveryID]; job.ClaimRequestKey != "" || job.Status != StatusAwaitingAgent || len(job.ReportTokenHash) != 0 {
		t.Fatalf("a failed claim changed the job: %+v", job)
	}

	repos.setFail(false)
	first, err := s.Claim(ctx, created.RecoveryID, created.ClaimToken, "key-a")
	if err != nil || first.Repository.SecretKey != "test-secret" {
		t.Fatalf("claim after recovery: %+v, %v", first.Repository, err)
	}

	// A redelivery that cannot open the repository keeps the claim and the
	// report token already issued.
	hash := store.jobs[created.RecoveryID].ReportTokenHash
	repos.setFail(true)
	if _, err := s.Claim(ctx, created.RecoveryID, created.ClaimToken, "key-a"); err == nil {
		t.Fatal("redelivery succeeded without repository access")
	}
	if job := store.jobs[created.RecoveryID]; job.ClaimRequestKey != "key-a" || !reflect.DeepEqual(job.ReportTokenHash, hash) {
		t.Fatalf("a failed redelivery changed the claim: %+v", job)
	}
}

func TestGetFindsAJobByEitherID(t *testing.T) {
	s := newTestService(newFakeStore(), testTTL, testRedelivery)
	created := createJob(t, s)
	ctx := context.Background()
	for _, id := range []string{created.RestoreID, created.RecoveryID} {
		job, err := s.Get(ctx, id)
		if err != nil || job.RestoreID != created.RestoreID {
			t.Fatalf("get %s: %s, %v", id, job.RestoreID, err)
		}
	}
	if _, err := s.Get(ctx, "0000000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown ID: err = %v, want ErrNotFound", err)
	}
}

// A failure in the phase that changes nothing on disk leaves the job open, and
// a second run of the same job — progress numbered from 1 again — proceeds.
func TestPreparingFailureCanBeRunAgain(t *testing.T) {
	store := newFakeStore()
	s := newTestService(store, testTTL, testRedelivery)
	created, tok := claimJob(t, s)
	ctx := context.Background()

	if err := s.RecordProgress(ctx, created.RecoveryID, tok, Progress{Seq: 3, Phase: "preparing", Step: 1, TotalSteps: 6}); err != nil {
		t.Fatal(err)
	}
	status, err := s.RecordResult(ctx, created.RecoveryID, tok, Result{Outcome: OutcomeFailed, Phase: "preparing", Message: "tool missing"})
	if err != nil || status != StatusClaimed {
		t.Fatalf("preparing failure: status %s, err %v; want claimed", status, err)
	}
	if job := store.jobs[created.RecoveryID]; job.Outcome != "" || job.Message != "tool missing" {
		t.Fatalf("job after preparing failure: %+v", job)
	}

	if err := s.RecordProgress(ctx, created.RecoveryID, tok, Progress{Seq: 1, Phase: "formatting", Step: 2, TotalSteps: 6}); err != nil {
		t.Fatalf("progress of the second run: %v", err)
	}
	if job := store.jobs[created.RecoveryID]; job.Status != StatusRestoring || job.Progress.Phase != "formatting" {
		t.Fatalf("second run not recorded: %+v", job)
	}

	// A failure after formatting is final.
	status, err = s.RecordResult(ctx, created.RecoveryID, tok, Result{Outcome: OutcomeFailed, Phase: "extracting", Message: "cut"})
	if err != nil || status != StatusFailed {
		t.Fatalf("later failure: status %s, err %v; want failed", status, err)
	}
	if _, err := s.RecordResult(ctx, created.RecoveryID, tok, Result{Outcome: OutcomeFailed, Phase: "preparing", Message: "x"}); !errors.Is(err, ErrResultConflict) {
		t.Fatalf("preparing failure after the job ended: err = %v, want ErrResultConflict", err)
	}
}
