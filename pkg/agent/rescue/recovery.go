// Package rescue implements the agent's work inside the rescue environment:
// claiming this instance's restore job from the framework and staging the
// restore plan and repository access it returns.
package rescue

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/spf13/viper"

	"github.com/cloud-barista/ai-ha/pkg/agent/transport"
)

var (
	// recoveryIDPattern is strict because the recovery ID becomes a directory
	// name: nothing like "../" may reach the path.
	recoveryIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	claimTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,256}$`)
)

// Recovery is the per-instance information the init script left for the
// rescue agent in recovery.env.
type Recovery struct {
	RecoveryID string
	// FrameworkURL is the https base URL of the framework, without a trailing slash.
	FrameworkURL string
	ClaimToken   string
	// CertSHA256 is the pinned SHA-256 of the framework certificate's DER
	// bytes. It is nil when pinning is off, and the framework is then verified
	// against the system's CA bundle.
	CertSHA256 []byte
}

// LoadRecovery reads and validates a recovery.env file of KEY=VALUE lines.
// Errors never include the claim token.
func LoadRecovery(path string) (Recovery, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("env")
	if err := v.ReadInConfig(); err != nil {
		return Recovery{}, fmt.Errorf("read recovery file %s: %w", path, err)
	}

	rec := Recovery{
		RecoveryID:   v.GetString("aiha_recovery_id"),
		FrameworkURL: strings.TrimRight(v.GetString("aiha_framework_url"), "/"),
		ClaimToken:   v.GetString("aiha_claim_token"),
	}
	if !recoveryIDPattern.MatchString(rec.RecoveryID) {
		return Recovery{}, errors.New("AIHA_RECOVERY_ID is missing or not 1-64 letters, digits and hyphens")
	}
	u, err := url.Parse(rec.FrameworkURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return Recovery{}, errors.New("AIHA_FRAMEWORK_URL must be an https URL")
	}
	if !claimTokenPattern.MatchString(rec.ClaimToken) {
		return Recovery{}, errors.New("AIHA_CLAIM_TOKEN is missing or malformed")
	}
	pinned, err := transport.ParseCertPin(v.GetString("aiha_framework_cert_pin"), "AIHA_FRAMEWORK_CERT_PIN")
	if err != nil {
		return Recovery{}, err
	}
	sha := strings.TrimSpace(v.GetString("aiha_framework_cert_sha256"))
	if !pinned {
		// A pin alongside "off" is a contradiction, not something to ignore.
		if sha != "" {
			return Recovery{}, errors.New("AIHA_FRAMEWORK_CERT_SHA256 must be empty when AIHA_FRAMEWORK_CERT_PIN is false")
		}
		return rec, nil
	}
	pin, err := hex.DecodeString(sha)
	if err != nil || len(pin) != 32 {
		return Recovery{}, errors.New("AIHA_FRAMEWORK_CERT_SHA256 must be 64 hexadecimal digits")
	}
	rec.CertSHA256 = pin
	return rec, nil
}
