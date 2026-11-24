package rest

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/agent"
	"github.com/cloud-barista/ai-ha/pkg/framework/ha"
	"github.com/cloud-barista/ai-ha/pkg/framework/inventory"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/cloud-barista/ai-ha/pkg/framework/repository"
	"github.com/cloud-barista/ai-ha/pkg/framework/restore"
	"github.com/cloud-barista/ai-ha/pkg/framework/secret"
	"github.com/cloud-barista/ai-ha/pkg/framework/token"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	testAdminToken = "test-admin-token-0123456789abcdef"
	testSystem     = "sys-1"
	testAgent      = "agent-1"
	testRepository = "aaaaaaaaaaaaaaaa"
	testSet        = "9c0d1e2f3a4b5c6d"
)

// memJobStore holds what the management routes read and write. The rules of the
// queue are covered in package job; what is under test here is the HTTP layer.
// Every other Store method stays nil, so a route reaching one fails the test.
type memJobStore struct {
	job.Store
	mu       sync.Mutex
	jobs     []job.Job
	attempts map[string]*job.Attempt
	filter   job.ListFilter
	fail     error
}

func (s *memJobStore) Insert(_ context.Context, j job.NewJob, ttl time.Duration) (job.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	created := job.Job{
		JobID: j.JobID, SystemID: j.SystemID, Kind: j.Kind, Status: job.StatusQueued, Spec: j.Spec,
		RequestedBy: j.RequestedBy, CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	s.jobs = append(s.jobs, created)
	return created, nil
}

func (s *memJobStore) List(_ context.Context, f job.ListFilter) ([]job.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filter = f
	return slices.Clone(s.jobs), s.fail
}

func (s *memJobStore) ListBackups(_ context.Context, f job.ListFilter) ([]job.BackupRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filter = f
	var out []job.BackupRecord
	for _, j := range s.jobs {
		out = append(out, job.BackupRecord{Job: j, Attempt: s.attempts[j.CurrentAttemptID]})
	}
	return out, s.fail
}

func (s *memJobStore) FindBackups(_ context.Context, setID string) ([]job.BackupRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []job.BackupRecord
	for _, j := range s.jobs {
		a := s.attempts[j.CurrentAttemptID]
		if a == nil {
			continue
		}
		var r model.BackupJobResult
		if err := json.Unmarshal(a.Result, &r); err == nil && r.BackupSetID == setID {
			out = append(out, job.BackupRecord{Job: j, Attempt: a})
		}
	}
	return out, nil
}

func (s *memJobStore) FindCompletedBackupSet(_ context.Context, setID string) ([]job.BackupSetOrigin, error) {
	found, err := s.FindBackups(context.Background(), setID)
	if err != nil {
		return nil, err
	}
	var out []job.BackupSetOrigin
	for _, r := range found {
		if r.Job.Status == job.StatusCompleted {
			out = append(out, job.BackupSetOrigin{JobID: r.Job.JobID, SystemID: r.Job.SystemID, RepositoryID: testRepository})
		}
	}
	return out, nil
}

func (s *memJobStore) Get(_ context.Context, jobID string) (job.Job, *job.Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.JobID == jobID {
			return j, s.attempts[j.CurrentAttemptID], nil
		}
	}
	return job.Job{}, nil, job.ErrJobNotFound
}

func (s *memJobStore) RequestCancel(_ context.Context, jobID string) (job.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.jobs {
		if s.jobs[i].JobID != jobID {
			continue
		}
		switch s.jobs[i].Status {
		case job.StatusQueued:
			s.jobs[i].Status = job.StatusCancelled
		case job.StatusRunning:
			s.jobs[i].Status = job.StatusCancelling
		}
		return s.jobs[i], nil
	}
	return job.Job{}, job.ErrJobNotFound
}

// add seeds a backup job, with an attempt reporting setID when one is given.
func (s *memJobStore) add(jobID string, status job.Status, setID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := job.Job{
		JobID: jobID, SystemID: testSystem, Kind: job.KindBackup, Status: status, RequestedBy: "admin",
		Spec:      json.RawMessage(`{"devices":["/dev/sda1"],"compression":"zstd","repositoryId":"` + testRepository + `"}`),
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	if setID != "" {
		j.CurrentAttemptID = "attempt-" + jobID
		s.attempts[j.CurrentAttemptID] = &job.Attempt{
			AttemptID: j.CurrentAttemptID, JobID: jobID, AgentID: testAgent, ClaimRequestKey: "secret-claim-key",
			Phase: "archiving", ProcessedBytes: 4096, LastSeenAt: time.Now(), LeaseExpiresAt: time.Now().Add(time.Minute),
			Outcome: job.OutcomeCompleted,
			Result: json.RawMessage(`{"backupSetId":"` + setID + `","manifest":"` + setID +
				`-manifest.json","status":"completed","items":3}`),
		}
	}
	s.jobs = append(s.jobs, j)
}

// memRepoStore is an in-memory repository.Store.
type memRepoStore struct {
	mu   sync.Mutex
	recs map[string]repository.Record
}

func (s *memRepoStore) Insert(_ context.Context, r repository.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.recs {
		if existing.Definition.Name == r.Definition.Name {
			return repository.ErrExists
		}
	}
	s.recs[r.ID] = r
	return nil
}

func (s *memRepoStore) Get(_ context.Context, idOrName string) (repository.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.recs {
		if r.ID == idOrName || r.Definition.Name == idOrName {
			return r, nil
		}
	}
	return repository.Record{}, repository.ErrNotFound
}

func (s *memRepoStore) List(context.Context) ([]repository.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]repository.Record, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, r)
	}
	return out, nil
}

func (s *memRepoStore) SetCredentials(_ context.Context, id string, sealed []byte, hasSessionToken bool) (repository.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[id]
	if !ok {
		return repository.Record{}, repository.ErrNotFound
	}
	r.Credentials, r.HasSessionToken = sealed, hasSessionToken
	s.recs[id] = r
	return r, nil
}

// memRestoreStore holds what the management routes read and write.
type memRestoreStore struct {
	restore.Store
	mu     sync.Mutex
	jobs   []restore.Job
	filter restore.ListFilter
}

func (s *memRestoreStore) Insert(_ context.Context, j restore.Job, ttl time.Duration) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j.ClaimExpiresAt = time.Now().Add(ttl)
	s.jobs = append(s.jobs, j)
	return j.ClaimExpiresAt, nil
}

func (s *memRestoreStore) Lookup(_ context.Context, id string) (restore.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.RestoreID == id || j.RecoveryID == id {
			return j, nil
		}
	}
	return restore.Job{}, restore.ErrNotFound
}

func (s *memRestoreStore) List(_ context.Context, f restore.ListFilter) ([]restore.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filter = f
	return slices.Clone(s.jobs), nil
}

var testFramework = model.FrameworkAccess{URL: "https://fw.example.com:8443", CertPin: true, CertSHA256: "abc123"}

type mgmtEnv struct {
	h        http.Handler
	cfg      Config
	agents   *agentStore
	jobs     *memJobStore
	inv      *inventoryStore
	repos    *memRepoStore
	restores *memRestoreStore
}

func newMgmtEnv(t *testing.T) *mgmtEnv {
	t.Helper()
	box, err := secret.NewBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("box: %v", err)
	}

	env := &mgmtEnv{
		agents:   newAgentStore(testRegistrationToken),
		jobs:     &memJobStore{attempts: map[string]*job.Attempt{}},
		inv:      &inventoryStore{},
		repos:    &memRepoStore{recs: map[string]repository.Record{}},
		restores: &memRestoreStore{},
	}
	env.repos.recs[testRepository] = repository.Record{
		ID: testRepository, Type: repository.TypeS3,
		Definition: repository.Definition{Name: "minio-main", Provider: repository.ProviderMinio, Bucket: "aiha-backup"},
	}

	repos := repository.NewService(env.repos, box)
	jobs := job.NewService(env.jobs, repos, nil, job.TTLs{Backup: time.Hour, HA: 5 * time.Minute}, time.Minute,
		job.Timings{ProgressInterval: 10 * time.Second, LeaseTTL: 90 * time.Second})
	env.cfg = Config{
		Agent:     agent.NewService(env.agents),
		Jobs:      jobs,
		Inventory: inventory.NewService(env.inv),
		HA: ha.NewService(&haStore{}, nil, ha.Timings{ReportInterval: 30 * time.Second, StaleAfter: 90 * time.Second},
			func() string { return "0000000000000001" }),
		Repositories:    repos,
		Restore:         restore.NewService(env.restores, jobs, repos, time.Hour, time.Minute),
		AdminAuth:       StaticAdminToken(token.Hash(testAdminToken)),
		FrameworkAccess: testFramework,
		EnableSwagger:   true,
	}
	env.h = NewHandler(env.cfg)
	return env
}

// addAgent enrols an online agent directly.
func (e *mgmtEnv) addAgent(systemID, agentID string) *agent.Agent {
	seen := e.agents.now
	a := &agent.Agent{
		AgentID: agentID, SystemID: systemID, Hostname: "web01", AgentVersion: "aiha-agent 0.1.0",
		Mode: agent.ModeNormal, RegisteredAt: e.agents.now, LastSeenAt: &seen,
	}
	e.agents.agents[agentID] = a
	return a
}

// reportInventory stores a report with one partition the agent can back up.
func (e *mgmtEnv) reportInventory(t *testing.T, complete bool, age time.Duration) {
	t.Helper()
	err := e.inv.Put(context.Background(), testSystem, testAgent, inventory.Inventory{
		CollectedAt: time.Now().Add(-age),
		Complete:    complete,
		Partitions: []inventory.Partition{
			{Device: "/dev/sda1", MountPoint: "/", FSType: "ext4", FSUUID: "uuid-1", Candidate: true, Selectable: true},
			{Device: "/dev/sda2", MountPoint: "/mnt/x", FSType: "btrfs", Reason: "btrfs is not supported"},
		},
	})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
}

// clientAddress varies the caller's address so that a test walking many routes
// is not stopped by the rate limiter, which has its own tests.
var clientAddress atomic.Int64

func request(h http.Handler, method, path, authorization, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	n := clientAddress.Add(1)
	req.RemoteAddr = fmt.Sprintf("198.18.%d.%d:4000", n/250, n%250)
	if body != "" {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	if authorization != "" {
		req.Header.Set(echo.HeaderAuthorization, authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// call sends a request as an authenticated administrator.
func (e *mgmtEnv) call(method, path, body string) *httptest.ResponseRecorder {
	return request(e.h, method, path, "Bearer "+testAdminToken, body)
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return v
}

func expectFailure(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
	if got := decode[model.SimpleMsg](t, rec); got.Code != code || got.Message == "" {
		t.Fatalf("failure = %+v, want code %s and a message", got, code)
	}
}

func expectStatus(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
}

// expectAbsent fails if any of the given strings appears in a response.
func expectAbsent(t *testing.T, rec *httptest.ResponseRecorder, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(rec.Body.String(), s) {
			t.Errorf("%q appears in the response: %s", s, rec.Body)
		}
	}
}

func isManagementRoute(path string) bool {
	return !strings.HasPrefix(path, "/aiha/agent/") &&
		path != "/aiha/readyz" && path != "/aiha/swagger.json"
}

func TestManagementAPIClosedWithoutAuthenticator(t *testing.T) {
	env := newMgmtEnv(t)
	env.cfg.AdminAuth = nil

	for _, r := range newEcho(env.cfg).Routes() {
		if isManagementRoute(r.Path) {
			t.Errorf("%s %s is served with no authenticator", r.Method, r.Path)
		}
	}
	h := NewHandler(env.cfg)
	expectStatus(t, request(h, http.MethodGet, "/aiha/systems", "Bearer "+testAdminToken, ""), http.StatusNotFound)
}

// Walks the routes rather than listing them, so that a route added later
// cannot slip in without the middleware.
func TestEveryManagementRouteRequiresTheAdminToken(t *testing.T) {
	env := newMgmtEnv(t)
	var checked int
	for _, r := range newEcho(env.cfg).Routes() {
		if !isManagementRoute(r.Path) {
			continue
		}
		checked++
		path := strings.NewReplacer(":systemId", "s", ":agentId", "a", ":tokenId", "t", ":backupSetId", "b",
			":jobId", "j", ":restoreId", "r", ":repoId", "x", ":groupId", "g", ":incidentId", "i").Replace(r.Path)

		for _, authorization := range []string{"", "Bearer wrong-token", "Basic " + testAdminToken} {
			rec := request(env.h, r.Method, path, authorization, "")
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %q: status %d", r.Method, path, authorization, rec.Code)
				continue
			}
			if got := decode[model.SimpleMsg](t, rec); got.Code != model.CodeUnauthorized {
				t.Errorf("%s %s: code = %s", r.Method, path, got.Code)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no management route was found to check")
	}
}

// The two credentials must not open each other's doors.
func TestCredentialsAreNotInterchangeable(t *testing.T) {
	env := newMgmtEnv(t)
	enrolled := enrol(t, env.h)

	if rec := request(env.h, http.MethodGet, "/aiha/systems", "Bearer "+enrolled.AgentToken, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("an agent token opened the management API: status %d", rec.Code)
	}
	if rec := getJobs(env.h, "Bearer "+testAdminToken, "?wait=5"); rec.Code != http.StatusUnauthorized {
		t.Errorf("the admin token passed as an agent: status %d", rec.Code)
	}
}

// An authenticator that cannot decide must not look like a rejected credential.
func TestAdminAuthOutageIsRetryable(t *testing.T) {
	env := newMgmtEnv(t)
	env.cfg.AdminAuth = func(context.Context, string) (Principal, error) {
		return Principal{}, errors.New("identity provider unreachable")
	}
	rec := request(NewHandler(env.cfg), http.MethodGet, "/aiha/systems", "Bearer anything", "")
	expectFailure(t, rec, http.StatusServiceUnavailable, model.CodeUnavailable)
	if rec.Header().Get(echo.HeaderRetryAfter) == "" {
		t.Error("no Retry-After on a retryable failure")
	}
}

// Middleware attached to a group would answer this with 401.
func TestUnknownPathIsNotFoundWithoutAToken(t *testing.T) {
	env := newMgmtEnv(t)
	expectStatus(t, request(env.h, http.MethodGet, "/aiha/no-such-route", "", ""), http.StatusNotFound)
}

func TestUnimplementedRoutesAnswer501(t *testing.T) {
	env := newMgmtEnv(t)
	tests := []struct{ method, path string }{
		{http.MethodPost, "/aiha/systems/s/inventory/refresh"},
		{http.MethodPut, "/aiha/systems/s/targets"},
		{http.MethodDelete, "/aiha/backups/b"},
		{http.MethodPost, "/aiha/restores/r/cancel"},
		{http.MethodPost, "/aiha/repositories/x/verify"},
		{http.MethodGet, "/aiha/ha/groups"},
		{http.MethodPost, "/aiha/ha/groups"},
		{http.MethodGet, "/aiha/ha/groups/g"},
		{http.MethodPost, "/aiha/ha/groups/g/setup"},
		{http.MethodPost, "/aiha/ha/groups/g/teardown"},
		{http.MethodPost, "/aiha/ha/groups/g/switchover"},
		{http.MethodPost, "/aiha/ha/groups/g/failover"},
		{http.MethodPost, "/aiha/ha/groups/g/resume"},
		{http.MethodPost, "/aiha/ha/groups/g/rejoin"},
		{http.MethodPost, "/aiha/ha/groups/g/acknowledge"},
		{http.MethodGet, "/aiha/ha/groups/g/transitions"},
		{http.MethodGet, "/aiha/ha/events"},
		{http.MethodGet, "/aiha/incidents"},
		{http.MethodPost, "/aiha/incidents"},
		{http.MethodGet, "/aiha/incidents/i"},
		{http.MethodPost, "/aiha/incidents/i/events"},
		{http.MethodPost, "/aiha/incidents/i/diagnoses"},
		{http.MethodPost, "/aiha/incidents/i/alarms"},
		{http.MethodPost, "/aiha/incidents/i/actions"},
		{http.MethodGet, "/aiha/history"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			expectFailure(t, env.call(tt.method, tt.path, ""), http.StatusNotImplemented, model.CodeNotImplemented)
		})
	}

	expectFailure(t, request(env.h, http.MethodPost, "/aiha/agent/restore/r/boot-verify", "", ""),
		http.StatusNotImplemented, model.CodeNotImplemented)
}

func TestSystems(t *testing.T) {
	env := newMgmtEnv(t)

	empty := env.call(http.MethodGet, "/aiha/systems", "")
	expectStatus(t, empty, http.StatusOK)
	if !strings.Contains(empty.Body.String(), `"items":[]`) {
		t.Errorf("an empty list must be [], not null: %s", empty.Body)
	}

	env.addAgent(testSystem, testAgent)
	list := decode[model.ProtectedSystemListResp](t, env.call(http.MethodGet, "/aiha/systems", ""))
	if len(list.Items) != 1 || list.Items[0].SystemID != testSystem || list.Items[0].AgentStatus != agent.StateOnline {
		t.Fatalf("systems = %+v", list)
	}

	before := decode[model.ProtectedSystemDetail](t, env.call(http.MethodGet, "/aiha/systems/"+testSystem, ""))
	if before.AgentID != testAgent || before.Inventory != nil {
		t.Fatalf("before a report: %+v", before)
	}

	env.reportInventory(t, true, time.Minute)
	after := decode[model.ProtectedSystemDetail](t, env.call(http.MethodGet, "/aiha/systems/"+testSystem, ""))
	if after.Inventory == nil || !after.Inventory.Complete || after.Inventory.Stale {
		t.Fatalf("after a report: %+v", after.Inventory)
	}

	expectFailure(t, env.call(http.MethodGet, "/aiha/systems/no-such-system", ""), http.StatusNotFound, model.CodeNotFound)
}

func TestSystemInventory(t *testing.T) {
	env := newMgmtEnv(t)
	expectFailure(t, env.call(http.MethodGet, "/aiha/systems/"+testSystem+"/inventory", ""),
		http.StatusNotFound, model.CodeNoInventory)

	env.reportInventory(t, false, 48*time.Hour)
	got := decode[model.SystemInventoryInfo](t, env.call(http.MethodGet, "/aiha/systems/"+testSystem+"/inventory", ""))
	if got.Complete || !got.Stale || got.AgeSeconds < 1 || len(got.Report.Partitions) != 2 {
		t.Fatalf("inventory = %+v", got)
	}
}

func TestBackupChosenFromInventory(t *testing.T) {
	env := newMgmtEnv(t)
	env.addAgent(testSystem, testAgent)
	env.reportInventory(t, true, time.Minute)

	rec := env.call(http.MethodPost, "/aiha/systems/"+testSystem+"/backups", `{"repositoryId":"minio-main"}`)
	expectStatus(t, rec, http.StatusAccepted)
	got := decode[model.BackupAcceptedResp](t, rec)
	if len(got.Devices) != 1 || got.Devices[0] != "/dev/sda1" || got.Status != "queued" || len(got.Warnings) != 0 {
		t.Fatalf("response = %+v", got)
	}
	if want := "/aiha/jobs/" + got.JobID; rec.Header().Get(echo.HeaderLocation) != want {
		t.Errorf("Location = %q, want %q", rec.Header().Get(echo.HeaderLocation), want)
	}

	stored := env.jobs.jobs[0]
	if stored.RequestedBy != adminSubject {
		t.Errorf("requested by %q", stored.RequestedBy)
	}
	var spec job.BackupSpec
	if err := json.Unmarshal(stored.Spec, &spec); err != nil {
		t.Fatalf("spec: %v", err)
	}
	// The name is resolved to the ID, and the UUID is what lets the agent notice
	// a reused device path before it writes.
	if spec.RepositoryID != testRepository || spec.ExpectedFSUUIDs["/dev/sda1"] != "uuid-1" {
		t.Fatalf("spec = %+v", spec)
	}
}

func TestBackupRefusals(t *testing.T) {
	path := "/aiha/systems/" + testSystem + "/backups"
	const body = `{"repositoryId":"minio-main"}`

	tests := []struct {
		name   string
		setup  func(*testing.T, *mgmtEnv)
		path   string
		body   string
		status int
		code   string
	}{
		{"unknown system", nil, "/aiha/systems/no-such-system/backups", body,
			http.StatusNotFound, model.CodeNotFound},
		{"no repository", nil, path, `{}`, http.StatusBadRequest, model.CodeInvalidRequest},
		{"malformed", nil, path, `{`, http.StatusBadRequest, model.CodeInvalidRequest},
		{"unknown compression", nil, path, `{"repositoryId":"minio-main","compression":"lz4"}`,
			http.StatusBadRequest, model.CodeInvalidRequest},
		{"repeated device", nil, path, `{"repositoryId":"minio-main","devices":["/dev/sda1","/dev/sda1"]}`,
			http.StatusBadRequest, model.CodeInvalidRequest},
		{"no inventory to choose from", nil, path, body, http.StatusConflict, model.CodeNoInventory},
		{"incomplete inventory", func(t *testing.T, e *mgmtEnv) { e.reportInventory(t, false, time.Minute) },
			path, body, http.StatusConflict, model.CodeInventoryIncomplete},
		{"device not reported", func(t *testing.T, e *mgmtEnv) { e.reportInventory(t, true, time.Minute) },
			path, `{"repositoryId":"minio-main","devices":["/dev/sdz9"]}`,
			http.StatusBadRequest, model.CodeDeviceNotBackupable},
		// Listed with a reason, which is an explanation and not a permission.
		{"device listed as unusable", func(t *testing.T, e *mgmtEnv) { e.reportInventory(t, true, time.Minute) },
			path, `{"repositoryId":"minio-main","devices":["/dev/sda2"]}`,
			http.StatusBadRequest, model.CodeDeviceNotBackupable},
		{"unknown repository", func(t *testing.T, e *mgmtEnv) { e.reportInventory(t, true, time.Minute) },
			path, `{"repositoryId":"no-such-repository"}`, http.StatusBadRequest, model.CodeRepositoryNotFound},
		{"no active agent", func(_ *testing.T, e *mgmtEnv) {
			revoked := e.agents.now
			e.agents.agents[testAgent].RevokedAt = &revoked
		}, path, body, http.StatusConflict, model.CodeConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newMgmtEnv(t)
			env.addAgent(testSystem, testAgent)
			if tt.setup != nil {
				tt.setup(t, env)
			}
			expectFailure(t, env.call(http.MethodPost, tt.path, tt.body), tt.status, tt.code)
			if len(env.jobs.jobs) != 0 {
				t.Fatalf("a refused request queued a job: %+v", env.jobs.jobs)
			}
		})
	}
}

// The stated exception in protocol.md 2.7: named partitions go through with no
// report to check them against, and the caller is told so.
func TestBackupNamedWithoutInventoryWarns(t *testing.T) {
	env := newMgmtEnv(t)
	env.addAgent(testSystem, testAgent)

	rec := env.call(http.MethodPost, "/aiha/systems/"+testSystem+"/backups",
		`{"repositoryId":"minio-main","devices":["/dev/sda1"]}`)
	expectStatus(t, rec, http.StatusAccepted)
	if got := decode[model.BackupAcceptedResp](t, rec); len(got.Warnings) != 1 {
		t.Fatalf("warnings = %v", got.Warnings)
	}
}

func TestBackupOnStaleInventoryWarns(t *testing.T) {
	env := newMgmtEnv(t)
	env.addAgent(testSystem, testAgent)
	env.reportInventory(t, true, 48*time.Hour)

	rec := env.call(http.MethodPost, "/aiha/systems/"+testSystem+"/backups", `{"repositoryId":"minio-main"}`)
	expectStatus(t, rec, http.StatusAccepted)
	if got := decode[model.BackupAcceptedResp](t, rec); len(got.Warnings) != 1 {
		t.Fatalf("warnings = %v", got.Warnings)
	}
}

func TestBackupHistory(t *testing.T) {
	env := newMgmtEnv(t)
	env.jobs.add("queued-job", job.StatusQueued, "")
	env.jobs.add("done-job", job.StatusCompleted, testSet)

	rec := env.call(http.MethodGet, "/aiha/backups?systemId="+testSystem+
		"&status=completed&from=2026-01-01T00:00:00Z&to=2027-01-01T00:00:00Z&limit=10&offset=5", "")
	expectStatus(t, rec, http.StatusOK)

	f := env.jobs.filter
	if f.SystemID != testSystem || f.Status != job.StatusCompleted || f.Limit != 10 || f.Offset != 5 ||
		f.From.IsZero() || f.To.IsZero() {
		t.Fatalf("filter = %+v", f)
	}

	items := decode[model.BackupListResp](t, rec).Items
	if len(items) != 2 {
		t.Fatalf("%d items", len(items))
	}
	if items[0].BackupSetID != "" || len(items[0].Devices) != 1 {
		t.Errorf("a backup that has not run: %+v", items[0])
	}
	if got := items[1]; got.BackupSetID != testSet || got.Items != 3 || got.ProcessedBytes != 4096 ||
		got.SetStatus != "completed" || got.RepositoryID != testRepository {
		t.Errorf("a finished backup: %+v", got)
	}
}

func TestBackupByID(t *testing.T) {
	env := newMgmtEnv(t)
	env.jobs.add("done-job", job.StatusCompleted, testSet)

	got := decode[model.BackupInfo](t, env.call(http.MethodGet, "/aiha/backups/"+testSet, ""))
	if got.JobID != "done-job" || got.Manifest != testSet+"-manifest.json" {
		t.Fatalf("backup = %+v", got)
	}
	expectFailure(t, env.call(http.MethodGet, "/aiha/backups/no-such-set", ""), http.StatusNotFound, model.CodeNotFound)

	env.jobs.add("other-job", job.StatusCompleted, testSet)
	expectFailure(t, env.call(http.MethodGet, "/aiha/backups/"+testSet, ""), http.StatusConflict, model.CodeBackupSetAmbiguous)
}

// One row in an unexpected shape must not cost a caller the rest of the history.
func TestBackupHistorySurvivesAnUnreadableResult(t *testing.T) {
	env := newMgmtEnv(t)
	env.jobs.add("done-job", job.StatusCompleted, testSet)
	env.jobs.attempts["attempt-done-job"].Result = json.RawMessage(`"not an object"`)

	items := decode[model.BackupListResp](t, env.call(http.MethodGet, "/aiha/backups", "")).Items
	if len(items) != 1 || items[0].JobID != "done-job" || items[0].BackupSetID != "" {
		t.Fatalf("items = %+v", items)
	}
}

func TestListParameters(t *testing.T) {
	env := newMgmtEnv(t)

	expectStatus(t, env.call(http.MethodGet, "/aiha/jobs", ""), http.StatusOK)
	if env.jobs.filter.Limit != defaultListLimit {
		t.Errorf("default limit = %d", env.jobs.filter.Limit)
	}

	for _, query := range []string{
		"limit=0", "limit=201", "limit=many", "offset=-1", "offset=x", "from=yesterday", "to=2026-13-01",
		"from=2026-02-01T00:00:00Z&to=2026-01-01T00:00:00Z", "status=lost", "kind=restore",
	} {
		t.Run(query, func(t *testing.T) {
			expectFailure(t, env.call(http.MethodGet, "/aiha/jobs?"+query, ""),
				http.StatusBadRequest, model.CodeInvalidRequest)
		})
	}
	expectFailure(t, env.call(http.MethodGet, "/aiha/restores?status=lost", ""),
		http.StatusBadRequest, model.CodeInvalidRequest)
}

func TestJobRead(t *testing.T) {
	env := newMgmtEnv(t)
	env.jobs.add("done-job", job.StatusCompleted, testSet)

	list := env.call(http.MethodGet, "/aiha/jobs?kind=backup", "")
	if items := decode[model.JobListResp](t, list).Items; len(items) != 1 || items[0].Attempt != nil {
		t.Fatalf("items = %+v", items)
	}

	rec := env.call(http.MethodGet, "/aiha/jobs/done-job", "")
	expectStatus(t, rec, http.StatusOK)
	got := decode[model.JobInfo](t, rec)
	if got.Attempt == nil || got.Attempt.ProcessedBytes != 4096 || !got.Attempt.TotalUnknown {
		t.Fatalf("job = %+v", got)
	}
	expectAbsent(t, rec, "secret-claim-key", "leaseToken", "claimRequestKey")

	expectFailure(t, env.call(http.MethodGet, "/aiha/jobs/no-such-job", ""), http.StatusNotFound, model.CodeNotFound)
}

func TestJobCancel(t *testing.T) {
	env := newMgmtEnv(t)
	env.jobs.add("queued-job", job.StatusQueued, "")
	env.jobs.add("running-job", job.StatusRunning, "")
	env.jobs.add("done-job", job.StatusCompleted, "")

	expectStatus(t, env.call(http.MethodPost, "/aiha/jobs/queued-job/cancel", ""), http.StatusOK)
	// Already cancelled: cancelling is safe to repeat.
	expectStatus(t, env.call(http.MethodPost, "/aiha/jobs/queued-job/cancel", ""), http.StatusOK)
	// Only marked: the agent has yet to confirm it stopped.
	expectStatus(t, env.call(http.MethodPost, "/aiha/jobs/running-job/cancel", ""), http.StatusAccepted)
	expectStatus(t, env.call(http.MethodPost, "/aiha/jobs/running-job/cancel", ""), http.StatusAccepted)

	expectFailure(t, env.call(http.MethodPost, "/aiha/jobs/done-job/cancel", ""), http.StatusConflict, model.CodeJobFinished)
	expectFailure(t, env.call(http.MethodPost, "/aiha/jobs/no-such-job/cancel", ""), http.StatusNotFound, model.CodeNotFound)
}

// An HA job ends with its HA operation; cancelling it alone is refused and
// leaves it as it was.
func TestHAJobCancelIsRefused(t *testing.T) {
	env := newMgmtEnv(t)
	env.jobs.add("ha-job", job.StatusQueued, "")
	env.jobs.mu.Lock()
	env.jobs.jobs[0].Kind = job.KindHAPromote
	env.jobs.mu.Unlock()

	expectFailure(t, env.call(http.MethodPost, "/aiha/jobs/ha-job/cancel", ""), http.StatusConflict, model.CodeConflict)
	j, _, err := env.jobs.Get(context.Background(), "ha-job")
	if err != nil || j.Status != job.StatusQueued {
		t.Fatalf("job = %+v (%v), want still queued", j, err)
	}
}

func TestRestoreRequest(t *testing.T) {
	env := newMgmtEnv(t)
	env.jobs.add("done-job", job.StatusCompleted, testSet)

	rec := env.call(http.MethodPost, "/aiha/restores", `{"backupSetId":"`+testSet+`"}`)
	expectStatus(t, rec, http.StatusAccepted)
	got := decode[model.RestoreAcceptedResp](t, rec)
	if got.Status != string(restore.StatusAwaitingAgent) || got.Rescue.ClaimToken == "" ||
		got.Rescue.RecoveryID == "" || got.Rescue.Framework != testFramework {
		t.Fatalf("response = %+v", got)
	}
	if want := "/aiha/restores/" + got.RestoreID; rec.Header().Get(echo.HeaderLocation) != want {
		t.Errorf("Location = %q, want %q", rec.Header().Get(echo.HeaderLocation), want)
	}

	// The two IDs are kept apart: following a job must not reveal what the
	// rescue instance authenticates with, or accept what it was given.
	read := env.call(http.MethodGet, "/aiha/restores/"+got.RestoreID, "")
	expectStatus(t, read, http.StatusOK)
	expectAbsent(t, read, got.Rescue.ClaimToken, got.Rescue.RecoveryID)
	info := decode[model.RestoreInfo](t, read)
	if info.SourceSystemID != testSystem || info.SourceBackupSetID != testSet || info.RepositoryID != testRepository {
		t.Fatalf("restore = %+v", info)
	}

	expectFailure(t, env.call(http.MethodGet, "/aiha/restores/"+got.Rescue.RecoveryID, ""), http.StatusNotFound, model.CodeNotFound)
	expectFailure(t, env.call(http.MethodGet, "/aiha/restores/no-such-restore", ""), http.StatusNotFound, model.CodeNotFound)
}

func TestRestoreRefusals(t *testing.T) {
	env := newMgmtEnv(t)
	env.jobs.add("failed-job", job.StatusFailed, testSet)

	expectFailure(t, env.call(http.MethodPost, "/aiha/restores", `{}`), http.StatusBadRequest, model.CodeInvalidRequest)
	expectFailure(t, env.call(http.MethodPost, "/aiha/restores", `{`), http.StatusBadRequest, model.CodeInvalidRequest)
	expectFailure(t, env.call(http.MethodPost, "/aiha/restores", `{"backupSetId":"no-such-set"}`),
		http.StatusBadRequest, model.CodeBackupSetNotFound)
	// On record, but a set that failed is not something to restore from.
	expectFailure(t, env.call(http.MethodPost, "/aiha/restores", `{"backupSetId":"`+testSet+`"}`),
		http.StatusBadRequest, model.CodeBackupSetNotFound)

	if len(env.restores.jobs) != 0 {
		t.Fatalf("a refused request registered a job: %+v", env.restores.jobs)
	}
}

func TestRestoreProgressAndBootVerification(t *testing.T) {
	env := newMgmtEnv(t)
	env.restores.jobs = []restore.Job{
		{RestoreID: "waiting", Status: restore.StatusAwaitingAgent, Plan: json.RawMessage(`{}`)},
		{
			RestoreID: "restoring", Status: restore.StatusRestoring,
			Plan:     json.RawMessage(`{"sourceSystemId":"sys-1","sourceBackupSetId":"` + testSet + `"}`),
			Progress: restore.Progress{Seq: 4, Phase: "extracting", Step: 4, TotalSteps: 6, ProcessedBytes: 10},
		},
		{
			RestoreID: "restored", Status: restore.StatusAwaitingBoot, Outcome: restore.OutcomeSucceeded,
			Plan: json.RawMessage(`{"sourceSystemId":"sys-1","sourceBackupSetId":"` + testSet + `"}`),
		},
	}

	rec := env.call(http.MethodGet, "/aiha/restores?systemId=sys-1&backupSetId="+testSet+"&status=restoring&limit=5", "")
	expectStatus(t, rec, http.StatusOK)
	if f := env.restores.filter; f.SystemID != "sys-1" || f.BackupSetID != testSet ||
		f.Status != restore.StatusRestoring || f.Limit != 5 {
		t.Fatalf("filter = %+v", f)
	}

	items := decode[model.RestoreListResp](t, rec).Items
	if len(items) != 3 {
		t.Fatalf("%d items", len(items))
	}
	if items[0].Progress != nil || items[0].BootVerification != "" {
		t.Errorf("waiting: %+v", items[0])
	}
	if p := items[1].Progress; p == nil || p.Step != 4 || !p.TotalUnknown {
		t.Errorf("restoring: %+v", items[1].Progress)
	}
	if items[2].BootVerification != bootVerificationPending || items[2].RestoreOutcome != string(restore.OutcomeSucceeded) {
		t.Errorf("restored: %+v", items[2])
	}
}

func TestRepositories(t *testing.T) {
	const access, secretKey = "ACCESS-KEY-VALUE", "SECRET-KEY-VALUE"
	env := newMgmtEnv(t)

	body := `{"name":"aws-main","provider":"AWS","region":"ap-northeast-2","bucket":"aiha-backup",` +
		`"accessKey":"` + access + `","secretKey":"` + secretKey + `"}`
	rec := env.call(http.MethodPost, "/aiha/repositories", body)
	expectStatus(t, rec, http.StatusCreated)
	expectAbsent(t, rec, access, secretKey)
	created := decode[model.RepositoryInfo](t, rec)
	if rec.Header().Get(echo.HeaderLocation) != "/aiha/repositories/"+created.ID {
		t.Errorf("Location = %q", rec.Header().Get(echo.HeaderLocation))
	}

	// What is stored must be the sealed form, not the request.
	stored := env.repos.recs[created.ID].Credentials
	if strings.Contains(string(stored), secretKey) {
		t.Fatal("the credentials were stored in plaintext")
	}

	expectFailure(t, env.call(http.MethodPost, "/aiha/repositories", body), http.StatusConflict, model.CodeRepositoryExists)
	expectFailure(t, env.call(http.MethodPost, "/aiha/repositories", `{"name":"x"}`), http.StatusBadRequest, model.CodeInvalidRequest)
	expectFailure(t, env.call(http.MethodPost, "/aiha/repositories",
		`{"name":"x","provider":"AWS","bucket":"aiha-backup","accessKey":"a","secretKey":"s"}`),
		http.StatusBadRequest, model.CodeInvalidRequest)

	list := env.call(http.MethodGet, "/aiha/repositories", "")
	if items := decode[model.RepositoryListResp](t, list).Items; len(items) != 2 {
		t.Fatalf("%d repositories", len(items))
	}
	expectAbsent(t, list, access, secretKey)

	byName := decode[model.RepositoryInfo](t, env.call(http.MethodGet, "/aiha/repositories/aws-main", ""))
	if byName.ID != created.ID {
		t.Fatalf("by name: %+v", byName)
	}
	expectFailure(t, env.call(http.MethodGet, "/aiha/repositories/no-such-repository", ""), http.StatusNotFound, model.CodeNotFound)

	rec = env.call(http.MethodPut, "/aiha/repositories/"+created.ID+"/credentials",
		`{"accessKey":"new-access","secretKey":"new-secret","sessionToken":"new-session"}`)
	expectStatus(t, rec, http.StatusOK)
	expectAbsent(t, rec, "new-access", "new-secret", "new-session")
	if !decode[model.RepositoryInfo](t, rec).HasSessionToken {
		t.Error("the session token was not recorded")
	}
	if string(env.repos.recs[created.ID].Credentials) == string(stored) {
		t.Error("the credentials were not replaced")
	}

	expectFailure(t, env.call(http.MethodPut, "/aiha/repositories/no-such-repository/credentials",
		`{"accessKey":"a","secretKey":"s"}`), http.StatusNotFound, model.CodeNotFound)
	expectFailure(t, env.call(http.MethodPut, "/aiha/repositories/"+created.ID+"/credentials", `{}`),
		http.StatusBadRequest, model.CodeInvalidRequest)
}

func TestAgents(t *testing.T) {
	env := newMgmtEnv(t)
	env.addAgent(testSystem, testAgent)

	got := decode[model.AgentListResp](t, env.call(http.MethodGet, "/aiha/agents", ""))
	if len(got.Items) != 1 || got.Items[0].Status != agent.StateOnline || got.Items[0].Hostname != "web01" {
		t.Fatalf("agents = %+v", got)
	}

	expectStatus(t, env.call(http.MethodPost, "/aiha/agents/"+testAgent+"/revoke", ""), http.StatusOK)
	if env.agents.agents[testAgent].RevokedAt == nil {
		t.Fatal("the agent was not revoked")
	}
	got = decode[model.AgentListResp](t, env.call(http.MethodGet, "/aiha/agents", ""))
	if got.Items[0].Status != agent.StateRevoked {
		t.Fatalf("status = %s", got.Items[0].Status)
	}

	expectFailure(t, env.call(http.MethodPost, "/aiha/agents/no-such-agent/revoke", ""), http.StatusNotFound, model.CodeNotFound)
}

func TestRegistrationTokens(t *testing.T) {
	env := newMgmtEnv(t)

	// An empty request is valid, and the limits it gets are the narrow ones.
	rec := env.call(http.MethodPost, "/aiha/registration-tokens", "")
	expectStatus(t, rec, http.StatusCreated)
	issued := decode[model.RegistrationTokenIssuedResp](t, rec)
	if issued.Token == "" || issued.MaxUses != defaultTokenMaxUses || issued.Framework != testFramework {
		t.Fatalf("issued = %+v", issued)
	}
	if ttl := issued.ExpiresAt.Sub(env.agents.now); ttl != defaultTokenTTLSeconds*time.Second {
		t.Errorf("ttl = %s", ttl)
	}

	custom := decode[model.RegistrationTokenIssuedResp](t, env.call(http.MethodPost, "/aiha/registration-tokens",
		`{"ttlSeconds":60,"maxUses":3,"note":"rollout"}`))
	if custom.MaxUses != 3 {
		t.Fatalf("custom = %+v", custom)
	}

	for _, body := range []string{`{"ttlSeconds":-1}`, `{"maxUses":-1}`, `{"ttlSeconds":999999999}`} {
		expectFailure(t, env.call(http.MethodPost, "/aiha/registration-tokens", body),
			http.StatusBadRequest, model.CodeInvalidRequest)
	}

	list := env.call(http.MethodGet, "/aiha/registration-tokens", "")
	expectAbsent(t, list, issued.Token, custom.Token)
	items := decode[model.RegistrationTokenListResp](t, list).Items
	if len(items) != 2 || items[0].Status != agent.TokenStateUsable {
		t.Fatalf("tokens = %+v", items)
	}

	expectStatus(t, env.call(http.MethodPost, "/aiha/registration-tokens/"+issued.TokenID+"/revoke", ""), http.StatusOK)
	expectFailure(t, env.call(http.MethodPost, "/aiha/registration-tokens/no-such-token/revoke", ""),
		http.StatusNotFound, model.CodeNotFound)
}

// Secrets pass through these handlers on their way in or out. None of them, the
// admin token included, may reach a log line.
func TestManagementHandlersDoNotLogSecrets(t *testing.T) {
	var buf strings.Builder
	prev := log.Logger
	log.Logger = zerolog.New(&buf).Level(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = prev })

	const access, secretKey = "ACCESS-KEY-VALUE", "SECRET-KEY-VALUE"
	env := newMgmtEnv(t)
	env.jobs.add("done-job", job.StatusCompleted, testSet)

	repo := `{"name":"aws-main","provider":"AWS","region":"ap-northeast-2","bucket":"aiha-backup",` +
		`"accessKey":"` + access + `","secretKey":"` + secretKey + `"}`
	env.call(http.MethodPost, "/aiha/repositories", repo)
	env.call(http.MethodPost, "/aiha/repositories", repo) // conflict
	env.call(http.MethodPost, "/aiha/repositories",
		`{"name":"bad name","provider":"AWS","bucket":"aiha-backup","accessKey":"`+access+`","secretKey":"`+secretKey+`"}`)
	env.call(http.MethodPut, "/aiha/repositories/no-such-repository/credentials",
		`{"accessKey":"`+access+`","secretKey":"`+secretKey+`"}`)
	env.call(http.MethodPost, "/aiha/repositories", `{"accessKey":`+secretKey) // malformed
	token := decode[model.RegistrationTokenIssuedResp](t, env.call(http.MethodPost, "/aiha/registration-tokens", ""))
	restored := decode[model.RestoreAcceptedResp](t, env.call(http.MethodPost, "/aiha/restores", `{"backupSetId":"`+testSet+`"}`))
	request(env.h, http.MethodGet, "/aiha/systems", "Bearer wrong-secret-value", "")

	logs := buf.String()
	if logs == "" {
		t.Fatal("nothing was logged, so this test proved nothing")
	}
	for _, secret := range []string{
		access, secretKey, testAdminToken, "wrong-secret-value", token.Token, restored.Rescue.ClaimToken,
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("a secret appears in the logs: %q\n%s", secret, logs)
		}
	}
}

// A contract that drifts from the code misleads every client written against
// it, so the two are compared in both directions. "make swag" fixes a failure.
func TestSwaggerMatchesTheRoutes(t *testing.T) {
	env := newMgmtEnv(t)

	served := map[string]bool{}
	for _, r := range newEcho(env.cfg).Routes() {
		segments := strings.Split(strings.TrimPrefix(r.Path, "/aiha"), "/")
		for i, s := range segments {
			if name, ok := strings.CutPrefix(s, ":"); ok {
				segments[i] = "{" + name + "}"
			}
		}
		served[strings.ToLower(r.Method)+" "+strings.Join(segments, "/")] = true
	}

	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(swaggerSpec, &doc); err != nil {
		t.Fatalf("decode the document: %v", err)
	}
	documented := map[string]bool{}
	for path, operations := range doc.Paths {
		for method := range operations {
			documented[method+" "+path] = true
		}
	}

	for route := range served {
		if !documented[route] {
			t.Errorf("%s is served but not documented", route)
		}
	}
	for route := range documented {
		if !served[route] {
			t.Errorf("%s is documented but not served", route)
		}
	}
}

// The digest is what the server holds, so presenting it must not work either.
func TestStaticAdminTokenMatchesOnlyItsToken(t *testing.T) {
	auth := StaticAdminToken(token.Hash(testAdminToken))

	got, err := auth(context.Background(), testAdminToken)
	if err != nil || got.Subject != adminSubject {
		t.Fatalf("principal = %+v, err = %v", got, err)
	}
	for _, wrong := range []string{"", testAdminToken + "x", hex.EncodeToString(token.Hash(testAdminToken))} {
		if _, err := auth(context.Background(), wrong); !errors.Is(err, ErrAdminTokenRejected) {
			t.Errorf("%q: err = %v", wrong, err)
		}
	}
}
