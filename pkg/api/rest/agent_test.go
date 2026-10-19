package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/agent"
	"github.com/cloud-barista/ai-ha/pkg/framework/token"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// agentStore is the minimum in-memory Store the handlers need. Enrolment
// semantics are covered in package agent; what is under test here is the HTTP
// layer on top of them.
type agentStore struct {
	mu          sync.Mutex
	now         time.Time
	tokenHash   string
	tokenUsed   bool
	agents      map[string]*agent.Agent
	agentTokens map[string]string
	failAuth    error
	touched     int
}

func newAgentStore(registrationToken string) *agentStore {
	return &agentStore{
		now:         time.Now(),
		tokenHash:   string(token.Hash(registrationToken)),
		agents:      map[string]*agent.Agent{},
		agentTokens: map[string]string{},
	}
}

func (s *agentStore) InsertRegistrationToken(context.Context, string, []byte, time.Duration, int, string) (time.Time, error) {
	return s.now, nil
}

func (s *agentStore) Register(_ context.Context, tokenHash []byte, a agent.NewAgent) (agent.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if string(tokenHash) != s.tokenHash {
		return agent.Agent{}, agent.ErrTokenRejected
	}
	for _, existing := range s.agents {
		if existing.RegistrationKey == a.RegistrationKey {
			return *existing, agent.ErrAlreadyRegistered
		}
	}
	if s.tokenUsed {
		return agent.Agent{}, agent.ErrTokenRejected
	}
	s.tokenUsed = true

	created := &agent.Agent{
		AgentID: a.AgentID, SystemID: a.SystemID, Mode: a.Mode,
		RegistrationKey: a.RegistrationKey, RegisteredAt: s.now,
	}
	s.agents[a.AgentID] = created
	s.agentTokens[string(a.TokenHash)] = a.AgentID
	return *created, nil
}

func (s *agentStore) AuthenticateAgent(_ context.Context, tokenHash []byte) (agent.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAuth != nil {
		return agent.Agent{}, s.failAuth
	}
	id, ok := s.agentTokens[string(tokenHash)]
	if !ok {
		return agent.Agent{}, agent.ErrAgentNotFound
	}
	a := *s.agents[id]
	if a.RevokedAt != nil {
		return a, agent.ErrAgentRevoked
	}
	return a, nil
}

func (s *agentStore) TouchAgent(_ context.Context, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched++
	if a, ok := s.agents[agentID]; ok {
		seen := s.now
		a.LastSeenAt = &seen
	}
	return nil
}

func (s *agentStore) ListAgents(context.Context) ([]agent.Agent, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]agent.Agent, 0, len(s.agents))
	for _, a := range s.agents {
		out = append(out, *a)
	}
	return out, s.now, nil
}

func (s *agentStore) RevokeAgent(_ context.Context, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.agents[agentID]; ok {
		revoked := s.now
		a.RevokedAt = &revoked
	}
	return nil
}

func (s *agentStore) ListRegistrationTokens(context.Context) ([]agent.RegistrationToken, time.Time, error) {
	return nil, s.now, nil
}

func (s *agentStore) RevokeRegistrationToken(context.Context, string) error { return nil }

const (
	testRegistrationToken = "test-registration-token"
	testRegistrationKey   = "0123456789abcdef0123456789abcdef"
)

func newAgentHandler(t *testing.T) (http.Handler, *agentStore) {
	t.Helper()
	store := newAgentStore(testRegistrationToken)
	return NewHandler(Config{Agent: agent.NewService(store)}), store
}

func postRegister(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/aiha/agent/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func registerBody(key string) string {
	b, _ := json.Marshal(model.AgentRegisterReq{
		RegistrationToken: testRegistrationToken,
		RegistrationKey:   key,
		SystemID:          "sys-0123456789abcdef",
		Hostname:          "web01",
	})
	return string(b)
}

func getJobs(h http.Handler, authorization, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/aiha/agent/jobs"+query, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAgentRegisterHandler(t *testing.T) {
	h, _ := newAgentHandler(t)

	rec := postRegister(h, registerBody(testRegistrationKey))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp model.AgentRegisterResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AgentID == "" || resp.AgentToken == "" {
		t.Fatalf("incomplete response: %+v", resp)
	}

	// The issued token authenticates the job endpoint.
	if code := getJobs(h, "Bearer "+resp.AgentToken, "?wait=5").Code; code != http.StatusNoContent {
		t.Fatalf("jobs status %d, want 204", code)
	}
}

func TestAgentRegisterHandlerFailures(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"malformed body", "{", http.StatusBadRequest},
		{"missing fields", `{"registrationToken":"x"}`, http.StatusBadRequest},
		{
			"bad system id",
			`{"registrationToken":"` + testRegistrationToken + `","registrationKey":"` + testRegistrationKey + `","systemId":"../escape"}`,
			http.StatusBadRequest,
		},
		{
			"wrong token",
			`{"registrationToken":"nope","registrationKey":"` + testRegistrationKey + `","systemId":"sys-1"}`,
			http.StatusUnauthorized,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newAgentHandler(t)
			rec := postRegister(h, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body)
			}
		})
	}
}

// A retry of a completed enrolment is a conflict, not an authentication
// failure: the operator has to reset the agent, not find a better token.
func TestAgentRegisterRetryIsAConflict(t *testing.T) {
	h, _ := newAgentHandler(t)

	if code := postRegister(h, registerBody(testRegistrationKey)).Code; code != http.StatusOK {
		t.Fatalf("first register: %d", code)
	}

	rec := postRegister(h, registerBody(testRegistrationKey))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409 (%s)", rec.Code, rec.Body)
	}
	var msg model.SimpleMsg
	_ = json.Unmarshal(rec.Body.Bytes(), &msg)
	if !strings.Contains(msg.Message, "already completed") {
		t.Errorf("message does not say the enrolment completed: %q", msg.Message)
	}
}

func TestAgentJobsRequiresAToken(t *testing.T) {
	h, store := newAgentHandler(t)

	for _, authorization := range []string{"", "Bearer ", "Basic abc", "not-a-scheme", "Bearer unknown-token"} {
		rec := getJobs(h, authorization, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status %d, want 401", authorization, rec.Code)
		}
	}
	if store.touched != 0 {
		t.Errorf("an unauthenticated caller recorded contact %d times", store.touched)
	}
}

// The scheme is case-insensitive per RFC 7235; the token is not.
func TestAgentJobsAcceptsAnyBearerCase(t *testing.T) {
	h, _ := newAgentHandler(t)
	rec := postRegister(h, registerBody(testRegistrationKey))
	var resp model.AgentRegisterResp
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	for _, scheme := range []string{"Bearer ", "bearer ", "BEARER "} {
		if code := getJobs(h, scheme+resp.AgentToken, "?wait=5").Code; code != http.StatusNoContent {
			t.Errorf("scheme %q: status %d, want 204", scheme, code)
		}
	}
}

func TestAgentJobsRejectsARevokedToken(t *testing.T) {
	h, store := newAgentHandler(t)
	rec := postRegister(h, registerBody(testRegistrationKey))
	var resp model.AgentRegisterResp
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	if err := store.RevokeAgent(context.Background(), resp.AgentID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	got := getJobs(h, "Bearer "+resp.AgentToken, "")
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", got.Code)
	}
	var msg model.SimpleMsg
	_ = json.Unmarshal(got.Body.Bytes(), &msg)
	if !strings.Contains(msg.Message, "register again") {
		t.Errorf("a revoked agent is not told to register again: %q", msg.Message)
	}
}

// A database outage must not look like a rejected credential. Conflating them
// would tell every healthy agent to discard its token during an outage.
func TestAgentJobsReportsAStoreOutageAsRetryable(t *testing.T) {
	h, store := newAgentHandler(t)
	rec := postRegister(h, registerBody(testRegistrationKey))
	var resp model.AgentRegisterResp
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	store.failAuth = errors.New("connection refused")

	got := getJobs(h, "Bearer "+resp.AgentToken, "")
	if got.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: a store outage was reported as an auth failure", got.Code)
	}
	if got.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After on a retryable failure")
	}
}

// Neither token may appear in a log line.
func TestAgentHandlersDoNotLogTokens(t *testing.T) {
	var buf strings.Builder
	prev := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = prev })

	h, _ := newAgentHandler(t)
	rec := postRegister(h, registerBody(testRegistrationKey))
	var resp model.AgentRegisterResp
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	postRegister(h, registerBody(testRegistrationKey)) // conflict
	postRegister(h, `{"registrationToken":"wrong-secret-value","registrationKey":"`+testRegistrationKey+`","systemId":"sys-1"}`)
	getJobs(h, "Bearer "+resp.AgentToken, "?wait=5")
	getJobs(h, "Bearer another-secret-value", "")

	logs := buf.String()
	for _, secret := range []string{
		testRegistrationToken, resp.AgentToken, "wrong-secret-value", "another-secret-value",
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("a token appears in the logs: %q\n%s", secret, logs)
		}
	}
}

func TestPollWaitClamps(t *testing.T) {
	tests := []struct {
		raw  string
		want time.Duration
	}{
		{"", maxPollWait},
		{"garbage", maxPollWait},
		{"0", minPollWait},   // the spin a caller would otherwise get
		{"-10", minPollWait}, // and the same by another route
		{"1", minPollWait},
		{"10", 10 * time.Second},
		{"45", maxPollWait},
		{"600", maxPollWait},
	}
	for _, tt := range tests {
		if got := pollWait(tt.raw); got != tt.want {
			t.Errorf("pollWait(%q) = %s, want %s", tt.raw, got, tt.want)
		}
	}
}

// An agent asking for no wait must still be made to wait, or the endpoint
// becomes a busy loop that the per-IP rate limit alone would not stop.
func TestAgentJobsCannotBeSpun(t *testing.T) {
	h, _ := newAgentHandler(t)
	rec := postRegister(h, registerBody(testRegistrationKey))
	var resp model.AgentRegisterResp
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	start := time.Now()
	if code := getJobs(h, "Bearer "+resp.AgentToken, "?wait=0").Code; code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", code)
	}
	if elapsed := time.Since(start); elapsed < minPollWait {
		t.Fatalf("a wait=0 poll returned after %s, faster than the %s floor", elapsed, minPollWait)
	}
}

// The job route must not reach the routes that carry no credential.
func TestAgentAuthDoesNotCoverPublicRoutes(t *testing.T) {
	h := NewHandler(Config{
		Agent:         agent.NewService(newAgentStore(testRegistrationToken)),
		EnableSwagger: true,
	})

	for _, path := range []string{"/aiha/readyz", "/aiha/swagger.json"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s requires an agent token", path)
		}
	}

	// Enrolment carries no agent token either: it is what issues one.
	if code := postRegister(h, registerBody(testRegistrationKey)).Code; code == http.StatusUnauthorized {
		t.Error("enrolment requires an agent token")
	}
}

// Without an agent service the routes must not exist at all.
func TestAgentRoutesAbsentWithoutService(t *testing.T) {
	h := NewHandler(Config{})

	if code := postRegister(h, registerBody(testRegistrationKey)).Code; code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("register served without an agent service: %d", code)
	}
	if code := getJobs(h, "Bearer x", "").Code; code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("jobs served without an agent service: %d", code)
	}
}

// The long poll has to outlast the server's own write timeout, which is set
// short to shed stalled connections. A handler test cannot show this: the
// deadline lives on the listener, so this one runs a real server.
func TestAgentJobsOutlastsTheServerWriteTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("takes longer than the server write timeout")
	}

	h, _ := newAgentHandler(t)
	rec := postRegister(h, registerBody(testRegistrationKey))
	var resp model.AgentRegisterResp
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	srv := httptest.NewUnstartedServer(h)
	applyTimeouts(srv.Config) // the same deadlines Run applies
	srv.Start()
	t.Cleanup(srv.Close)

	// Longer than writeTimeout, so an unextended deadline cuts the response.
	wait := writeTimeout + 5*time.Second
	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/aiha/agent/jobs?wait=%d", srv.URL, int(wait.Seconds())), nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+resp.AgentToken)

	client := &http.Client{Timeout: wait + 30*time.Second}
	start := time.Now()
	got, err := client.Do(req)
	if err != nil {
		t.Fatalf("the long poll was cut short after %s: %v", time.Since(start), err)
	}
	defer got.Body.Close()

	if got.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d, want 204", got.StatusCode)
	}
	// It must have held for the requested wait, which is past writeTimeout.
	if elapsed := time.Since(start); elapsed < wait {
		t.Fatalf("returned after %s, want at least the requested %s", elapsed, wait)
	}
}
