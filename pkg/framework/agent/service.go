package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	backupmodel "github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/token"
)

// offlineAfter is how long without contact makes an agent offline. An idle
// agent long-polls every 45s, so this leaves room for one lost poll and a
// backoff before its absence is reported.
const offlineAfter = 120 * time.Second

// systemIDPattern is what an enrolling agent may claim as its system id. It
// becomes a directory name in the backup repository, so it is restricted to
// characters that cannot escape a path.
var systemIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// registrationKeyPattern is the client-generated key for one enrolment
// attempt, in the same shape as the rescue agent's claim request key.
var registrationKeyPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Service issues registration tokens and enrols agents.
type Service struct {
	store Store
}

// NewService returns a Service backed by store.
func NewService(store Store) *Service {
	return &Service{store: store}
}

// IssuedToken describes a registration token an operator just created. Token
// is returned only here: the store keeps just its hash.
type IssuedToken struct {
	TokenID   string
	Token     string
	ExpiresAt time.Time
	MaxUses   int
}

// IssueRegistrationToken creates a token that authorizes enrolment, and
// nothing else, until it expires or its uses run out.
func (s *Service) IssueRegistrationToken(ctx context.Context, ttl time.Duration, maxUses int, note string) (IssuedToken, error) {
	switch {
	case ttl <= 0:
		return IssuedToken{}, fmt.Errorf("%w: the token lifetime must be positive", ErrInvalidRequest)
	case maxUses <= 0:
		return IssuedToken{}, fmt.Errorf("%w: the token must allow at least one use", ErrInvalidRequest)
	}

	plain, err := token.New()
	if err != nil {
		return IssuedToken{}, err
	}
	tokenID := backupmodel.NewID()

	expiresAt, err := s.store.InsertRegistrationToken(ctx, tokenID, token.Hash(plain), ttl, maxUses, note)
	if err != nil {
		return IssuedToken{}, err
	}
	return IssuedToken{TokenID: tokenID, Token: plain, ExpiresAt: expiresAt, MaxUses: maxUses}, nil
}

// Registration is what an agent asks to enrol as.
type Registration struct {
	// SystemID is the machine the agent protects. The agent proposes it; what
	// makes the enrolment legitimate is the registration token, never this.
	SystemID string
	// RegistrationKey identifies one enrolment attempt so that a retry is
	// recognised as a retry rather than enrolling a second agent.
	RegistrationKey string
	Hostname        string
	AgentVersion    string
}

// Registered is a completed enrolment. Token is returned only here.
type Registered struct {
	AgentID  string
	SystemID string
	Token    string
}

// Register spends a registration token and enrols an agent.
//
// The mode is set here rather than taken from the request: an agent that came
// up from a restored disk must not be able to declare itself a normal one.
func (s *Service) Register(ctx context.Context, registrationToken string, req Registration) (Registered, error) {
	if err := validateRegistration(req); err != nil {
		return Registered{}, err
	}
	if registrationToken == "" {
		return Registered{}, fmt.Errorf("%w: a registration token is required", ErrInvalidRequest)
	}

	agentToken, err := token.New()
	if err != nil {
		return Registered{}, err
	}

	created, err := s.store.Register(ctx, token.Hash(registrationToken), NewAgent{
		AgentID:         backupmodel.NewID(),
		SystemID:        req.SystemID,
		TokenHash:       token.Hash(agentToken),
		Hostname:        req.Hostname,
		AgentVersion:    req.AgentVersion,
		Mode:            ModeNormal,
		RegistrationKey: req.RegistrationKey,
	})
	if err != nil {
		// ErrAlreadyRegistered carries the existing agent, but its token was
		// only ever returned to the first caller and cannot be recovered.
		return Registered{}, err
	}

	return Registered{AgentID: created.AgentID, SystemID: created.SystemID, Token: agentToken}, nil
}

// Authenticate identifies the agent holding the presented token.
//
// It answers who the caller is and nothing more. Whether that agent may act on
// a particular job is a separate check, made where the job is known: holding a
// valid token must not by itself grant access to another agent's work.
func (s *Service) Authenticate(ctx context.Context, agentToken string) (Agent, error) {
	if agentToken == "" {
		return Agent{}, ErrAgentNotFound
	}
	return s.store.AuthenticateAgent(ctx, token.Hash(agentToken))
}

// Touch records that the agent was heard from.
func (s *Service) Touch(ctx context.Context, agentID string) error {
	return s.store.TouchAgent(ctx, agentID)
}

// AgentStatus is an enrolled agent with its liveness resolved.
type AgentStatus struct {
	Agent
	// Online is derived from LastSeenAt against the store's clock. The agent
	// never asserts this about itself.
	Online bool
}

// ListAgents returns the enrolled agents with their liveness.
func (s *Service) ListAgents(ctx context.Context) ([]AgentStatus, error) {
	agents, now, err := s.store.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]AgentStatus, 0, len(agents))
	for _, a := range agents {
		out = append(out, AgentStatus{Agent: a, Online: isOnline(a, now)})
	}
	return out, nil
}

// isOnline reports whether an agent has been heard from recently enough. A
// revoked agent is never online: it can no longer authenticate.
func isOnline(a Agent, now time.Time) bool {
	if a.RevokedAt != nil || a.LastSeenAt == nil {
		return false
	}
	return now.Sub(*a.LastSeenAt) < offlineAfter
}

// RevokeAgent revokes an agent's token, which takes effect on its next request.
func (s *Service) RevokeAgent(ctx context.Context, agentID string) error {
	if agentID == "" {
		return fmt.Errorf("%w: an agent id is required", ErrInvalidRequest)
	}
	return s.store.RevokeAgent(ctx, agentID)
}

// ListRegistrationTokens returns the issued tokens with their state.
func (s *Service) ListRegistrationTokens(ctx context.Context) ([]RegistrationToken, time.Time, error) {
	return s.store.ListRegistrationTokens(ctx)
}

// RevokeRegistrationToken revokes an unspent registration token.
func (s *Service) RevokeRegistrationToken(ctx context.Context, tokenID string) error {
	if tokenID == "" {
		return fmt.Errorf("%w: a token id is required", ErrInvalidRequest)
	}
	return s.store.RevokeRegistrationToken(ctx, tokenID)
}

// validateRegistration checks an enrolment request before a token is spent.
func validateRegistration(req Registration) error {
	switch {
	case !systemIDPattern.MatchString(req.SystemID):
		return fmt.Errorf("%w: the system id must be 1-64 characters of letters, digits, '-' or '_'", ErrInvalidRequest)
	case !registrationKeyPattern.MatchString(req.RegistrationKey):
		return fmt.Errorf("%w: the registration key must be 32 hexadecimal digits", ErrInvalidRequest)
	}
	return nil
}

// IsRejected reports whether err means the caller must obtain a new
// registration token rather than retry.
func IsRejected(err error) bool {
	return errors.Is(err, ErrTokenRejected) || errors.Is(err, ErrAlreadyRegistered)
}
