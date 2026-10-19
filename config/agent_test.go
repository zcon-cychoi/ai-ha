package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadAgentDefaults(t *testing.T) {
	cfg, err := LoadAgent()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.StateDir != "/var/lib/aiha" {
		t.Errorf("StateDir = %q", cfg.StateDir)
	}
	if cfg.PollWait != defaultPollWait {
		t.Errorf("PollWait = %s, want %s", cfg.PollWait, defaultPollWait)
	}
}

// The token may come from a file so that it need not sit in the environment of
// every process the agent starts.
func TestLoadAgentReadsTheTokenFromAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("  a-registration-token\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Setenv("AIHA_AGENT_REGISTRATION_TOKEN_FILE", path)
	t.Setenv("AIHA_AGENT_REGISTRATION_TOKEN", "from-the-environment")

	cfg, err := LoadAgent()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RegistrationToken != "a-registration-token" {
		t.Fatalf("token = %q, want the file's contents trimmed", cfg.RegistrationToken)
	}
}

func TestLoadAgentReportsAMissingTokenFile(t *testing.T) {
	t.Setenv("AIHA_AGENT_REGISTRATION_TOKEN_FILE", filepath.Join(t.TempDir(), "absent"))
	if _, err := LoadAgent(); err == nil {
		t.Fatal("a missing token file was accepted")
	}
}

func TestValidateFramework(t *testing.T) {
	const goodPin = "3911ba8d8fe5db35ae90469636dcd464c8b7aee357dc1bf9f9e6d8db3b77e81e"

	good := &AgentConfig{FrameworkURL: "https://fw:8443", PinFrameworkCert: true, FrameworkCertSHA256: goodPin}
	pin, err := good.ValidateFramework()
	if err != nil {
		t.Fatalf("a valid configuration was rejected: %v", err)
	}
	if len(pin) != 32 {
		t.Fatalf("pin is %d bytes, want 32", len(pin))
	}

	system := &AgentConfig{FrameworkURL: "https://fw.example.com"}
	pin, err = system.ValidateFramework()
	if err != nil || pin != nil {
		t.Fatalf("pinning off: pin %v, err %v; want no pin and no error", pin, err)
	}

	bad := []*AgentConfig{
		{PinFrameworkCert: true, FrameworkCertSHA256: goodPin},                                             // no URL
		{FrameworkURL: "http://fw:8443", PinFrameworkCert: true, FrameworkCertSHA256: goodPin},             // not https
		{FrameworkURL: "https://fw:8443", PinFrameworkCert: true},                                          // no pin
		{FrameworkURL: "https://fw:8443", PinFrameworkCert: true, FrameworkCertSHA256: "abc"},              // short
		{FrameworkURL: "https://fw:8443", PinFrameworkCert: true, FrameworkCertSHA256: "zz" + goodPin[2:]}, // not hex
		{FrameworkURL: "https://fw:8443", FrameworkCertSHA256: goodPin},                                    // pin given but pinning off
	}
	for _, cfg := range bad {
		if _, err := cfg.ValidateFramework(); err == nil {
			t.Errorf("ValidateFramework accepted %+v", cfg)
		}
	}
}

// A wait below the floor is reported rather than quietly raised: an agent
// polling far faster than intended is a load problem, and silently correcting
// it hides where it came from.
func TestResolvedPollWait(t *testing.T) {
	tests := []struct {
		wait    time.Duration
		want    time.Duration
		wantErr bool
	}{
		{0, 0, true},
		{-time.Second, 0, true},
		{time.Second, 0, true},
		{minPollWait, minPollWait, false},
		{20 * time.Second, 20 * time.Second, false},
		{maxPollWait, maxPollWait, false},
		{10 * time.Minute, maxPollWait, false},
	}
	for _, tt := range tests {
		got, err := (&AgentConfig{PollWait: tt.wait}).ResolvedPollWait()
		if (err != nil) != tt.wantErr {
			t.Errorf("PollWait %s: err = %v, wantErr %v", tt.wait, err, tt.wantErr)
			continue
		}
		if err == nil && got != tt.want {
			t.Errorf("PollWait %s resolved to %s, want %s", tt.wait, got, tt.want)
		}
	}

	// The rejection has to name the variable, or an operator cannot act on it.
	_, err := (&AgentConfig{PollWait: time.Second}).ResolvedPollWait()
	if err == nil || !strings.Contains(err.Error(), "AIHA_AGENT_POLL_WAIT") {
		t.Errorf("the error does not name the setting: %v", err)
	}
}

func TestLoadAgentCertPin(t *testing.T) {
	cfg, err := LoadAgent()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PinFrameworkCert {
		t.Error("pinning is off by default; it must be on")
	}

	t.Setenv("AIHA_AGENT_FRAMEWORK_CERT_PIN", "false")
	if cfg, err = LoadAgent(); err != nil || cfg.PinFrameworkCert {
		t.Errorf("false: pinning %v, err %v; want off", cfg != nil && cfg.PinFrameworkCert, err)
	}

	// A mistyped value must not be read as "off".
	t.Setenv("AIHA_AGENT_FRAMEWORK_CERT_PIN", "ture")
	if _, err := LoadAgent(); err == nil {
		t.Error("a mistyped value was accepted")
	}
}

func TestLoadFrameworkCertPin(t *testing.T) {
	cfg, err := LoadFramework()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.CertPin {
		t.Error("pinning is off by default; it must be on")
	}

	t.Setenv("AIHA_FRAMEWORK_CERT_PIN", "false")
	if cfg, err = LoadFramework(); err != nil || cfg.CertPin {
		t.Errorf("false: pinning %v, err %v; want off", cfg != nil && cfg.CertPin, err)
	}

	t.Setenv("AIHA_FRAMEWORK_CERT_PIN", "ture")
	if _, err := LoadFramework(); err == nil {
		t.Error("a mistyped value was accepted")
	}
}
