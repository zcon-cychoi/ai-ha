package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore is an in-memory Store with a settable clock. Register is atomic
// under mu, like the single statement in PGStore.
type fakeStore struct {
	mu     sync.Mutex
	now    time.Time
	tokens map[string]*storedToken // keyed by hash
	agents map[string]*Agent       // keyed by agent id
	// agentTokens maps an agent token hash to an agent id.
	agentTokens map[string]string
	// failAuth makes AuthenticateAgent report a store failure, standing in for
	// a database outage.
	failAuth error
}

type storedToken struct {
	RegistrationToken
	hash string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		now:         time.Now(),
		tokens:      map[string]*storedToken{},
		agents:      map[string]*Agent{},
		agentTokens: map[string]string{},
	}
}

func (f *fakeStore) InsertRegistrationToken(_ context.Context, tokenID string, hash []byte, ttl time.Duration, maxUses int, note string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	expiresAt := f.now.Add(ttl)
	f.tokens[string(hash)] = &storedToken{
		RegistrationToken: RegistrationToken{
			TokenID: tokenID, ExpiresAt: expiresAt, MaxUses: maxUses,
			Note: note, CreatedAt: f.now,
		},
		hash: string(hash),
	}
	return expiresAt, nil
}

func (f *fakeStore) Register(_ context.Context, tokenHash []byte, a NewAgent) (Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	t, ok := f.tokens[string(tokenHash)]
	spendable := ok && t.RevokedAt == nil && t.ExpiresAt.After(f.now) && t.Uses < t.MaxUses

	// A duplicate key is refused whether or not the token still has a use, and
	// refusing it must not spend one.
	if a.RegistrationKey != "" && ok {
		for _, existing := range f.agents {
			if existing.RegistrationTokenID == t.TokenID && existing.RegistrationKey == a.RegistrationKey {
				return *existing, ErrAlreadyRegistered
			}
		}
	}
	if !spendable {
		return Agent{}, ErrTokenRejected
	}

	t.Uses++
	created := &Agent{
		AgentID: a.AgentID, SystemID: a.SystemID, Hostname: a.Hostname,
		AgentVersion: a.AgentVersion, Mode: a.Mode,
		RegistrationTokenID: t.TokenID, RegistrationKey: a.RegistrationKey,
		RegisteredAt: f.now,
	}
	f.agents[a.AgentID] = created
	f.agentTokens[string(a.TokenHash)] = a.AgentID
	return *created, nil
}

func (f *fakeStore) AuthenticateAgent(_ context.Context, tokenHash []byte) (Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAuth != nil {
		return Agent{}, f.failAuth
	}
	id, ok := f.agentTokens[string(tokenHash)]
	if !ok {
		return Agent{}, ErrAgentNotFound
	}
	a := *f.agents[id]
	if a.RevokedAt != nil {
		return a, ErrAgentRevoked
	}
	return a, nil
}

func (f *fakeStore) TouchAgent(_ context.Context, agentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.agents[agentID]; ok {
		seen := f.now
		a.LastSeenAt = &seen
	}
	return nil
}

func (f *fakeStore) ListAgents(context.Context) ([]Agent, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Agent, 0, len(f.agents))
	for _, a := range f.agents {
		out = append(out, *a)
	}
	return out, f.now, nil
}

func (f *fakeStore) RevokeAgent(_ context.Context, agentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.agents[agentID]; ok && a.RevokedAt == nil {
		revoked := f.now
		a.RevokedAt = &revoked
	}
	return nil
}

func (f *fakeStore) ListRegistrationTokens(context.Context) ([]RegistrationToken, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RegistrationToken, 0, len(f.tokens))
	for _, t := range f.tokens {
		out = append(out, t.RegistrationToken)
	}
	return out, f.now, nil
}

func (f *fakeStore) RevokeRegistrationToken(_ context.Context, tokenID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tokens {
		if t.TokenID == tokenID && t.RevokedAt == nil {
			revoked := f.now
			t.RevokedAt = &revoked
		}
	}
	return nil
}

func (f *fakeStore) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fakeStore) usesOf(tokenID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tokens {
		if t.TokenID == tokenID {
			return t.Uses
		}
	}
	return -1
}

const testKey = "0123456789abcdef0123456789abcdef"

func testService(t *testing.T) (*Service, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	return NewService(store), store
}

func issue(t *testing.T, s *Service, ttl time.Duration, maxUses int) IssuedToken {
	t.Helper()
	issued, err := s.IssueRegistrationToken(context.Background(), ttl, maxUses, "test")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return issued
}

func registration(key string) Registration {
	return Registration{
		SystemID: "sys-0123456789abcdef", RegistrationKey: key,
		Hostname: "web01", AgentVersion: "test",
	}
}

func TestRegisterIssuesAnIdentityAndToken(t *testing.T) {
	svc, _ := testService(t)
	issued := issue(t, svc, time.Minute, 1)

	got, err := svc.Register(context.Background(), issued.Token, registration(testKey))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if got.AgentID == "" || got.Token == "" {
		t.Fatalf("incomplete registration: %+v", got)
	}
	if got.Token == issued.Token {
		t.Fatal("the agent token is the registration token")
	}

	// The issued token now authenticates, and reports the same identity.
	authed, err := svc.Authenticate(context.Background(), got.Token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if authed.AgentID != got.AgentID {
		t.Fatalf("authenticated as %s, registered as %s", authed.AgentID, got.AgentID)
	}
	// The framework decides the mode; the request never asked for one.
	if authed.Mode != ModeNormal {
		t.Fatalf("mode = %q, want %q", authed.Mode, ModeNormal)
	}
}

// A registration token authorizes enrolment and nothing else: it must not
// authenticate an agent.
func TestRegistrationTokenIsNotAnAgentToken(t *testing.T) {
	svc, _ := testService(t)
	issued := issue(t, svc, time.Minute, 1)

	if _, err := svc.Authenticate(context.Background(), issued.Token); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("a registration token authenticated as an agent: err = %v", err)
	}
}

func TestRegisterRejectsUnusableTokens(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		svc, _ := testService(t)
		_, err := svc.Register(context.Background(), "not-a-real-token", registration(testKey))
		if !errors.Is(err, ErrTokenRejected) {
			t.Fatalf("err = %v, want ErrTokenRejected", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		svc, store := testService(t)
		issued := issue(t, svc, time.Minute, 1)
		store.advance(2 * time.Minute)

		_, err := svc.Register(context.Background(), issued.Token, registration(testKey))
		if !errors.Is(err, ErrTokenRejected) {
			t.Fatalf("err = %v, want ErrTokenRejected", err)
		}
	})

	t.Run("used up", func(t *testing.T) {
		svc, _ := testService(t)
		issued := issue(t, svc, time.Minute, 1)
		if _, err := svc.Register(context.Background(), issued.Token, registration(testKey)); err != nil {
			t.Fatalf("first register: %v", err)
		}

		// A different key, so this is a second agent rather than a retry.
		_, err := svc.Register(context.Background(), issued.Token,
			registration(strings.Repeat("a", 32)))
		if !errors.Is(err, ErrTokenRejected) {
			t.Fatalf("err = %v, want ErrTokenRejected", err)
		}
	})

	t.Run("revoked", func(t *testing.T) {
		svc, _ := testService(t)
		issued := issue(t, svc, time.Minute, 5)
		if err := svc.RevokeRegistrationToken(context.Background(), issued.TokenID); err != nil {
			t.Fatalf("revoke: %v", err)
		}

		_, err := svc.Register(context.Background(), issued.Token, registration(testKey))
		if !errors.Is(err, ErrTokenRejected) {
			t.Fatalf("err = %v, want ErrTokenRejected", err)
		}
	})
}

// A retry of a successful enrolment must be told it already succeeded, not
// that its token expired: the two call for opposite responses from an operator.
func TestRegisterRefusesADuplicateKey(t *testing.T) {
	// Both paths matter: a single-use token (the use is gone, so nothing is
	// spendable) and a token with uses left (the duplicate is what stops it).
	for _, maxUses := range []int{1, 5} {
		t.Run(map[bool]string{true: "single-use", false: "uses remaining"}[maxUses == 1], func(t *testing.T) {
			svc, store := testService(t)
			issued := issue(t, svc, time.Minute, maxUses)

			if _, err := svc.Register(context.Background(), issued.Token, registration(testKey)); err != nil {
				t.Fatalf("first register: %v", err)
			}
			usesAfterFirst := store.usesOf(issued.TokenID)

			_, err := svc.Register(context.Background(), issued.Token, registration(testKey))
			if !errors.Is(err, ErrAlreadyRegistered) {
				t.Fatalf("err = %v, want ErrAlreadyRegistered", err)
			}

			// The refused retry must not have burned a use. This is what goes
			// wrong if the duplicate is swallowed instead of raised.
			if got := store.usesOf(issued.TokenID); got != usesAfterFirst {
				t.Fatalf("a refused retry spent a use: %d -> %d", usesAfterFirst, got)
			}
		})
	}
}

// Concurrent enrolments against a single-use token must produce exactly one
// agent.
func TestRegisterIsAtomicUnderConcurrency(t *testing.T) {
	svc, store := testService(t)
	issued := issue(t, svc, time.Minute, 1)

	const n = 16
	results := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			// Distinct keys, so each is a separate enrolment rather than a retry.
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
	if uses := store.usesOf(issued.TokenID); uses != 1 {
		t.Fatalf("token spent %d uses, want 1", uses)
	}
}

func TestRevokedAgentIsRejectedImmediately(t *testing.T) {
	svc, _ := testService(t)
	issued := issue(t, svc, time.Minute, 1)
	got, err := svc.Register(context.Background(), issued.Token, registration(testKey))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := svc.Authenticate(context.Background(), got.Token); err != nil {
		t.Fatalf("authenticate before revoke: %v", err)
	}

	if err := svc.RevokeAgent(context.Background(), got.AgentID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, err := svc.Authenticate(context.Background(), got.Token); !errors.Is(err, ErrAgentRevoked) {
		t.Fatalf("err = %v, want ErrAgentRevoked", err)
	}
}

// A store failure must stay distinguishable from a rejected token. Conflating
// them would tell every healthy agent to re-enrol during a database outage.
func TestAuthenticateDistinguishesStoreFailureFromRejection(t *testing.T) {
	svc, store := testService(t)
	issued := issue(t, svc, time.Minute, 1)
	got, err := svc.Register(context.Background(), issued.Token, registration(testKey))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	store.failAuth = errors.New("connection refused")

	_, err = svc.Authenticate(context.Background(), got.Token)
	if err == nil {
		t.Fatal("a store failure was reported as success")
	}
	if errors.Is(err, ErrAgentNotFound) || errors.Is(err, ErrAgentRevoked) {
		t.Fatalf("a store failure looks like a rejected token: %v", err)
	}
}

func TestOnlineFollowsLastContact(t *testing.T) {
	svc, store := testService(t)
	issued := issue(t, svc, time.Minute, 1)
	got, err := svc.Register(context.Background(), issued.Token, registration(testKey))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Never heard from: not online, even though it is enrolled.
	agents, err := svc.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if agents[0].Online {
		t.Error("an agent that has never called is online")
	}

	if err := svc.Touch(context.Background(), got.AgentID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	agents, _ = svc.ListAgents(context.Background())
	if !agents[0].Online {
		t.Error("an agent that just called is not online")
	}

	store.advance(offlineAfter + time.Second)
	agents, _ = svc.ListAgents(context.Background())
	if agents[0].Online {
		t.Error("an agent silent past the offline threshold is still online")
	}
}

func TestRegisterValidatesTheRequest(t *testing.T) {
	svc, _ := testService(t)
	issued := issue(t, svc, time.Minute, 5)

	bad := []Registration{
		{SystemID: "", RegistrationKey: testKey},
		{SystemID: "../escape", RegistrationKey: testKey},
		{SystemID: "has/slash", RegistrationKey: testKey},
		{SystemID: "sys-1", RegistrationKey: ""},
		{SystemID: "sys-1", RegistrationKey: "not-hex"},
		{SystemID: "sys-1", RegistrationKey: "abc"},
	}
	for _, req := range bad {
		if _, err := svc.Register(context.Background(), issued.Token, req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Register(%+v) err = %v, want ErrInvalidRequest", req, err)
		}
	}
	// A rejected request must not have spent the token.
	if _, err := svc.Register(context.Background(), issued.Token, registration(testKey)); err != nil {
		t.Fatalf("a valid request after rejected ones failed: %v", err)
	}
}

func TestIssueRegistrationTokenValidates(t *testing.T) {
	svc, _ := testService(t)
	if _, err := svc.IssueRegistrationToken(context.Background(), 0, 1, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("a zero lifetime was accepted: %v", err)
	}
	if _, err := svc.IssueRegistrationToken(context.Background(), time.Minute, 0, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("a zero use count was accepted: %v", err)
	}
}
