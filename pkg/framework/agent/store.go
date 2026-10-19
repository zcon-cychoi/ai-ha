// Package agent manages agent enrolment on the framework side: an operator
// issues a registration token, an agent presents it once to receive its own
// identity and long-lived token, and that token authenticates everything the
// agent does afterwards.
//
// The separation is deliberate. A registration token authorizes enrolment and
// nothing else; an agent token authorizes an agent's own work and cannot enrol
// anyone. Neither identifier — agentId or systemId — is a credential: what an
// enrolling client may attach itself to is decided by the registration token's
// grant, not by the ids the client sends.
package agent

import (
	"context"
	"errors"
	"time"
)

// Mode is how an agent is allowed to act.
type Mode string

const (
	// ModeNormal is an agent that may take ordinary work.
	ModeNormal Mode = "normal"
	// ModeRecoveryRegistration is an agent that came up from a restored disk
	// and must not take work belonging to the system it was restored from.
	// Reserved until the boot-verify path exists.
	ModeRecoveryRegistration Mode = "recovery_registration"
)

var (
	// ErrTokenRejected reports a registration token that is unknown, expired,
	// used up or revoked. The reasons are deliberately one error: telling an
	// unauthenticated caller which of them applies says more than it needs to.
	ErrTokenRejected = errors.New("registration token rejected")
	// ErrAlreadyRegistered reports that this registration key already enrolled
	// an agent. The caller is retrying an enrolment that in fact succeeded.
	ErrAlreadyRegistered = errors.New("registration already completed")
	// ErrAgentNotFound reports that no agent has the presented token.
	ErrAgentNotFound = errors.New("agent not found")
	// ErrAgentRevoked reports an agent whose token has been revoked.
	ErrAgentRevoked = errors.New("agent token revoked")
	// ErrInvalidRequest reports an enrolment request that cannot be accepted.
	ErrInvalidRequest = errors.New("invalid registration request")
)

// RegistrationToken is a token an operator issued for enrolment.
type RegistrationToken struct {
	TokenID   string
	ExpiresAt time.Time
	MaxUses   int
	Uses      int
	Note      string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// Agent is an enrolled agent as stored by the framework.
type Agent struct {
	AgentID      string
	SystemID     string
	Hostname     string
	AgentVersion string
	Mode         Mode
	// RegistrationTokenID and RegistrationKey record which enrolment produced
	// this agent, so that an operator can find it when the agent never
	// received the response and does not know its own AgentID.
	RegistrationTokenID string
	RegistrationKey     string
	RegisteredAt        time.Time
	LastSeenAt          *time.Time
	RevokedAt           *time.Time
}

// NewAgent is the row to create when a registration token is spent.
type NewAgent struct {
	AgentID         string
	SystemID        string
	TokenHash       []byte
	Hostname        string
	AgentVersion    string
	Mode            Mode
	RegistrationKey string
}

// Store persists registration tokens and agents. All time comparisons use the
// store's clock, so that an agent's clock cannot widen a token's window.
type Store interface {
	// InsertRegistrationToken stores a new token whose window closes ttl after
	// the store's current time. It returns that closing time.
	InsertRegistrationToken(ctx context.Context, tokenID string, hash []byte, ttl time.Duration, maxUses int, note string) (time.Time, error)

	// Register spends the registration token identified by tokenHash and
	// creates the agent, atomically: a token is never spent without an agent
	// to show for it, and an agent is never created without spending one.
	//
	// It returns ErrTokenRejected when the token cannot be spent and
	// ErrAlreadyRegistered when this registration key already enrolled an
	// agent.
	Register(ctx context.Context, tokenHash []byte, a NewAgent) (Agent, error)

	// AuthenticateAgent returns the agent holding tokenHash. It returns
	// ErrAgentNotFound when there is none and ErrAgentRevoked when the agent's
	// token has been revoked.
	AuthenticateAgent(ctx context.Context, tokenHash []byte) (Agent, error)

	// TouchAgent records that the agent was heard from, which is what liveness
	// is judged by.
	TouchAgent(ctx context.Context, agentID string) error

	// ListAgents returns the enrolled agents together with the store's current
	// time, so that "online" is derived from one clock.
	ListAgents(ctx context.Context) ([]Agent, time.Time, error)

	// RevokeAgent revokes an agent's token. It is idempotent.
	RevokeAgent(ctx context.Context, agentID string) error

	// ListRegistrationTokens returns issued tokens with the store's current
	// time. The tokens themselves are not stored and cannot be listed.
	ListRegistrationTokens(ctx context.Context) ([]RegistrationToken, time.Time, error)

	// RevokeRegistrationToken revokes an unspent token. It is idempotent.
	RevokeRegistrationToken(ctx context.Context, tokenID string) error
}
