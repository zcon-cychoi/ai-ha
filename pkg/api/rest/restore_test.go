package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/cloud-barista/ai-ha/pkg/framework/restore"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// stubStore holds a single job; enough to drive the handler through each
// status code. Claim semantics themselves are tested in package restore.
type stubStore struct {
	mu  sync.Mutex
	job restore.Job
	now time.Time
}

func (s *stubStore) Insert(_ context.Context, job restore.Job, ttl time.Duration) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job.ClaimExpiresAt = s.now.Add(ttl)
	s.job = job
	return job.ClaimExpiresAt, nil
}

func (s *stubStore) Get(_ context.Context, recoveryID string) (restore.Job, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if recoveryID != s.job.RecoveryID {
		return restore.Job{}, time.Time{}, restore.ErrNotFound
	}
	return s.job, s.now, nil
}

func (s *stubStore) TryClaim(_ context.Context, _, requestKey string, redelivery time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job.ClaimRequestKey != "" {
		return false, nil
	}
	until := s.now.Add(redelivery)
	s.job.Status, s.job.ClaimRequestKey, s.job.RedeliveryExpiresAt = restore.StatusClaimed, requestKey, &until
	return true, nil
}

func (s *stubStore) SetReportToken(_ context.Context, _, requestKey string, hash []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job.ClaimRequestKey != requestKey {
		return restore.ErrNotFound
	}
	s.job.ReportTokenHash = hash
	return nil
}

func (s *stubStore) UpdateProgress(_ context.Context, _ string, p restore.Progress) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job.Status != restore.StatusClaimed && s.job.Status != restore.StatusRestoring {
		return false, nil
	}
	s.job.Status, s.job.Progress = restore.StatusRestoring, p
	return true, nil
}

func (s *stubStore) Finish(_ context.Context, _ string, status restore.Status, r restore.Result) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job.Status != restore.StatusClaimed && s.job.Status != restore.StatusRestoring {
		return false, nil
	}
	s.job.Status, s.job.Outcome, s.job.Message = status, r.Outcome, r.Message
	return true, nil
}

func (s *stubStore) Reopen(_ context.Context, _ string, r restore.Result) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job.Status != restore.StatusClaimed && s.job.Status != restore.StatusRestoring {
		return false, nil
	}
	s.job.Status, s.job.Message = restore.StatusClaimed, r.Message
	return true, nil
}

func (s *stubStore) Lookup(_ context.Context, id string) (restore.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.job.RestoreID && id != s.job.RecoveryID {
		return restore.Job{}, restore.ErrNotFound
	}
	return s.job, nil
}

// stubSets knows the one backup set the handler tests restore.
type stubSets struct{}

func (stubSets) FindBackupSet(context.Context, string) (job.BackupSetOrigin, error) {
	return job.BackupSetOrigin{JobID: "job-1", SystemID: "web01", RepositoryID: "repo-1"}, nil
}

// stubRepos opens the stub repository.
type stubRepos struct{}

func (stubRepos) Access(context.Context, string) (model.RepositoryAccess, error) {
	return model.RepositoryAccess{
		Type: "s3", Provider: "Minio", Endpoint: "https://storage.example.com",
		Bucket: "b", AccessKey: "a", SecretKey: "s",
	}, nil
}

func newTestHandler(t *testing.T) (http.Handler, restore.Created) {
	t.Helper()
	svc := restore.NewService(&stubStore{now: time.Now()}, stubSets{}, stubRepos{}, time.Minute, time.Minute)
	created, err := svc.Create(context.Background(), restore.NewJob{BackupSetID: "set-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return NewHandler(Config{Restore: svc}), created
}

func postClaim(h http.Handler, recoveryID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/aiha/agent/restore/"+recoveryID+"/claim", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func claimBody(token, key string) string {
	b, _ := json.Marshal(model.RestoreClaimReq{ClaimToken: token, RequestKey: key})
	return string(b)
}

func TestRestoreClaimHandler(t *testing.T) {
	h, created := newTestHandler(t)
	id, token := created.RecoveryID, created.ClaimToken

	steps := []struct {
		name       string
		recoveryID string
		body       string
		wantStatus int
		wantMsg    string
	}{
		{"malformed body", id, "{", http.StatusBadRequest, "Malformed request body: check JSON syntax"},
		{"missing request key", id, claimBody(token, ""), http.StatusBadRequest, "Claim token and request key required"},
		{"unknown recovery", "no-such-id", claimBody(token, "key-a"), http.StatusNotFound, "Recovery not found"},
		{"wrong token", id, claimBody("wrong-token", "key-a"), http.StatusUnauthorized, "Claim token rejected"},
		{"first claim", id, claimBody(token, "key-a"), http.StatusOK, ""},
		{"same key again", id, claimBody(token, "key-a"), http.StatusOK, ""},
		{"other key", id, claimBody(token, "key-b"), http.StatusConflict, "Recovery already claimed"},
	}
	for _, s := range steps {
		rec := postClaim(h, s.recoveryID, s.body)
		if rec.Code != s.wantStatus {
			t.Fatalf("%s: status %d, want %d (%s)", s.name, rec.Code, s.wantStatus, rec.Body)
		}
		if s.wantStatus == http.StatusOK {
			var resp model.RestoreClaimResp
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.RecoveryID != id {
				t.Fatalf("%s: bad body %s (%v)", s.name, rec.Body, err)
			}
			continue
		}
		var msg model.SimpleMsg
		if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil || msg.Message != s.wantMsg {
			t.Fatalf("%s: message %q, want %q", s.name, rec.Body, s.wantMsg)
		}
	}
}

func TestRestoreClaimHandlerDoesNotLogToken(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = prev })

	h, created := newTestHandler(t)
	postClaim(h, created.RecoveryID, claimBody(created.ClaimToken, "key-a"))
	postClaim(h, created.RecoveryID, claimBody("wrong-token-value", "key-a"))

	if strings.Contains(buf.String(), created.ClaimToken) || strings.Contains(buf.String(), "wrong-token-value") {
		t.Fatalf("logs contain a claim token:\n%s", buf.String())
	}
}

func TestClaimFailureMapsWindowErrors(t *testing.T) {
	tests := []struct {
		err        error
		wantStatus int
		wantMsg    string
	}{
		{restore.ErrClaimExpired, http.StatusGone, "Claim window expired"},
		{restore.ErrRedeliveryExpired, http.StatusGone, "Claim response no longer available"},
		{errors.New("database down"), http.StatusInternalServerError, "Claim failed"},
	}
	for _, tt := range tests {
		if status, msg := claimFailure(tt.err); status != tt.wantStatus || msg != tt.wantMsg {
			t.Fatalf("claimFailure(%v) = %d %q, want %d %q", tt.err, status, msg, tt.wantStatus, tt.wantMsg)
		}
	}
}

func TestRestoreRoutesAbsentWithoutService(t *testing.T) {
	rec := postClaim(NewHandler(Config{}), "any", claimBody("t", "k"))
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d: restore route served without a restore service", rec.Code)
	}
}

func postReport(h http.Handler, recoveryID, kind, reportToken, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/aiha/agent/restore/"+recoveryID+"/"+kind, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if reportToken != "" {
		req.Header.Set("Authorization", "Bearer "+reportToken)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRestoreReportHandlers(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = prev })

	h, created := newTestHandler(t)
	id := created.RecoveryID
	claim := postClaim(h, id, claimBody(created.ClaimToken, "key-a"))
	var resp model.RestoreClaimResp
	if err := json.Unmarshal(claim.Body.Bytes(), &resp); err != nil || resp.ReportToken == "" {
		t.Fatalf("claim response has no report token: %s (%v)", claim.Body, err)
	}
	tok := resp.ReportToken

	progress := `{"seq":1,"phase":"extracting","step":4,"totalSteps":6,"processedBytes":10,"totalBytes":100}`
	result := `{"outcome":"succeeded","phase":"bootloader","message":"Restored"}`
	steps := []struct {
		name, kind, token, body string
		wantStatus              int
		wantMsg                 string
	}{
		{"progress without token", "progress", "", progress, http.StatusUnauthorized, "Report token required"},
		{"progress with wrong token", "progress", "wrong-report-token", progress, http.StatusUnauthorized, "Report token rejected"},
		{"progress without seq", "progress", tok, `{"phase":"extracting"}`, http.StatusBadRequest, "Sequence and phase required"},
		{"progress", "progress", tok, progress, http.StatusNoContent, ""},
		{"result with bad outcome", "result", tok, `{"outcome":"done"}`, http.StatusBadRequest, "Outcome must be succeeded or failed"},
		{"result", "result", tok, result, http.StatusNoContent, ""},
		{"same result again", "result", tok, result, http.StatusNoContent, ""},
		{"different result", "result", tok, `{"outcome":"failed"}`, http.StatusConflict, "A different result was already recorded for this restore"},
		{"progress after result", "progress", tok, `{"seq":2,"phase":"late"}`, http.StatusConflict, "Restore already finished"},
	}
	for _, s := range steps {
		rec := postReport(h, id, s.kind, s.token, s.body)
		if rec.Code != s.wantStatus {
			t.Fatalf("%s: status %d, want %d (%s)", s.name, rec.Code, s.wantStatus, rec.Body)
		}
		if s.wantMsg == "" {
			continue
		}
		var msg model.SimpleMsg
		if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil || msg.Message != s.wantMsg {
			t.Fatalf("%s: message %q, want %q", s.name, rec.Body, s.wantMsg)
		}
	}
	if strings.Contains(buf.String(), tok) || strings.Contains(buf.String(), "wrong-report-token") {
		t.Fatalf("logs contain a report token:\n%s", buf.String())
	}
}
