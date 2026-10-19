package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/cloud-barista/ai-ha/pkg/agent/transport"
)

// Poll wait bounds, matching what the framework clamps to. The floor exists so
// that an agent cannot be configured into a busy loop.
const (
	minPollWait     = 5 * time.Second
	maxPollWait     = 45 * time.Second
	defaultPollWait = 45 * time.Second
)

// Inventory reporting bounds. A system's partitions change rarely, so the
// default is generous; the floor is there because each report reads the disks,
// and a misconfiguration should not turn that into a loop.
const (
	minInventoryInterval     = 5 * time.Minute
	defaultInventoryInterval = time.Hour
)

// AgentConfig holds how the agent reaches the framework, loaded from
// AIHA_AGENT_* environment variables.
//
// It is separate from Config, which holds repository credentials, because the
// commands that talk to the framework have no use for those: an agent that
// only enrols and waits for work should not be reading S3 secrets.
type AgentConfig struct {
	// FrameworkURL is the https address of the framework.
	FrameworkURL string
	// PinFrameworkCert selects how the agent verifies the framework. True pins
	// the certificate named by FrameworkCertSHA256, for a self-signed
	// certificate. False verifies the standard way, against the system's CA
	// bundle and the host name in FrameworkURL, for a publicly trusted one.
	PinFrameworkCert bool
	// FrameworkCertSHA256 is the SHA-256 of the framework certificate's DER
	// bytes, which the agent pins. The framework prints it at startup. It must
	// be empty when PinFrameworkCert is false.
	FrameworkCertSHA256 string
	// RegistrationToken authorizes enrolment. It is a secret: never log it.
	// It is read from the environment or a file rather than a flag, because a
	// flag is visible to every process on the host through the process list.
	RegistrationToken string
	// StateDir is where the agent keeps its identity and credentials.
	StateDir string
	// PollWait is how long each long poll asks the framework to hold.
	PollWait time.Duration
	// InventoryInterval is how often the agent reports what its system is made
	// of, so that a backup can be asked for without naming partitions by hand.
	InventoryInterval time.Duration
	// LogLevel is the zerolog level ("debug", "info", "warn", "error").
	LogLevel string
}

// LoadAgent reads the agent's framework settings from AIHA_AGENT_*
// environment variables, applying defaults for anything not set.
func LoadAgent() (*AgentConfig, error) {
	v := viper.New()
	v.SetEnvPrefix("AIHA_AGENT")
	v.AutomaticEnv()

	v.SetDefault("state_dir", "/var/lib/aiha")
	v.SetDefault("poll_wait", defaultPollWait)
	v.SetDefault("inventory_interval", defaultInventoryInterval)
	v.SetDefault("log_level", "info")

	pin, err := transport.ParseCertPin(v.GetString("framework_cert_pin"), "AIHA_AGENT_FRAMEWORK_CERT_PIN")
	if err != nil {
		return nil, err
	}
	token, err := registrationToken(v)
	if err != nil {
		return nil, err
	}

	return &AgentConfig{
		FrameworkURL:        strings.TrimSpace(v.GetString("framework_url")),
		PinFrameworkCert:    pin,
		FrameworkCertSHA256: strings.TrimSpace(v.GetString("framework_cert_sha256")),
		RegistrationToken:   token,
		StateDir:            v.GetString("state_dir"),
		PollWait:            v.GetDuration("poll_wait"),
		InventoryInterval:   v.GetDuration("inventory_interval"),
		LogLevel:            v.GetString("log_level"),
	}, nil
}

// registrationToken reads the token from the environment, or from the file a
// second variable names. The file form keeps the secret out of the
// environment of every child process the agent starts.
func registrationToken(v *viper.Viper) (string, error) {
	if path := strings.TrimSpace(v.GetString("registration_token_file")); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read AIHA_AGENT_REGISTRATION_TOKEN_FILE: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(v.GetString("registration_token")), nil
}

// ValidateFramework checks the settings needed to reach the framework, and
// returns the decoded certificate pin, or nil when pinning is off.
//
// It is called before anything is sent, so that a mistyped pin is a message
// rather than a failed handshake.
func (c *AgentConfig) ValidateFramework() ([]byte, error) {
	if c.FrameworkURL == "" {
		return nil, fmt.Errorf("AIHA_AGENT_FRAMEWORK_URL is required")
	}
	if !strings.HasPrefix(c.FrameworkURL, "https://") {
		return nil, fmt.Errorf("AIHA_AGENT_FRAMEWORK_URL must be https, got %q", c.FrameworkURL)
	}
	if !c.PinFrameworkCert {
		// A pin alongside "off" is a contradiction, not something to ignore:
		// the operator may believe the certificate is still pinned.
		if c.FrameworkCertSHA256 != "" {
			return nil, fmt.Errorf("AIHA_AGENT_FRAMEWORK_CERT_SHA256 must be empty when AIHA_AGENT_FRAMEWORK_CERT_PIN is false")
		}
		return nil, nil
	}
	if c.FrameworkCertSHA256 == "" {
		return nil, fmt.Errorf("AIHA_AGENT_FRAMEWORK_CERT_SHA256 is required: the agent pins the framework certificate (or set AIHA_AGENT_FRAMEWORK_CERT_PIN=false for a publicly trusted certificate)")
	}

	pin, err := hex.DecodeString(c.FrameworkCertSHA256)
	if err != nil || len(pin) != 32 {
		return nil, fmt.Errorf("AIHA_AGENT_FRAMEWORK_CERT_SHA256 must be 64 hexadecimal digits")
	}
	return pin, nil
}

// ResolvedPollWait returns the poll wait, refusing a value that would make the
// agent poll in a tight loop.
//
// A zero or negative wait is rejected rather than quietly replaced: an agent
// polling far faster than intended is a load problem that should be reported
// where it was configured, not absorbed here.
func (c *AgentConfig) ResolvedPollWait() (time.Duration, error) {
	switch {
	case c.PollWait <= 0:
		return 0, fmt.Errorf("AIHA_AGENT_POLL_WAIT must be positive, got %s", c.PollWait)
	case c.PollWait < minPollWait:
		return 0, fmt.Errorf("AIHA_AGENT_POLL_WAIT must be at least %s, got %s", minPollWait, c.PollWait)
	case c.PollWait > maxPollWait:
		return maxPollWait, nil
	default:
		return c.PollWait, nil
	}
}

// ResolvedInventoryInterval returns how often to report the system's
// configuration, refusing a value that would have the agent read its disks in a
// tight loop.
//
// There is no upper bound. Reporting rarely is a decision an operator may make;
// the framework records when each inventory was collected, so a stale one is
// visible rather than silent.
func (c *AgentConfig) ResolvedInventoryInterval() (time.Duration, error) {
	switch {
	case c.InventoryInterval <= 0:
		return 0, fmt.Errorf("AIHA_AGENT_INVENTORY_INTERVAL must be positive, got %s", c.InventoryInterval)
	case c.InventoryInterval < minInventoryInterval:
		return 0, fmt.Errorf("AIHA_AGENT_INVENTORY_INTERVAL must be at least %s, got %s",
			minInventoryInterval, c.InventoryInterval)
	default:
		return c.InventoryInterval, nil
	}
}
