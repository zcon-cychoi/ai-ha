package cmd

import (
	"bytes"
	"testing"
)

// The printed lines are what the agent reads, so each mode must produce a
// combination the agent accepts: a fingerprint with pinning on, none with it off.
func TestPrintAgentEnv(t *testing.T) {
	const url, sha, token = "https://fw.example.com:8443", "3911ba8d8fe5db35ae90469636dcd464c8b7aee357dc1bf9f9e6d8db3b77e81e", "tok"

	tests := []struct {
		name string
		pin  bool
		sha  string
		want string
	}{
		{"pinned", true, sha, "AIHA_AGENT_FRAMEWORK_URL='" + url + "'\n" +
			"AIHA_AGENT_FRAMEWORK_CERT_PIN='true'\n" +
			"AIHA_AGENT_FRAMEWORK_CERT_SHA256='" + sha + "'\n" +
			"AIHA_AGENT_REGISTRATION_TOKEN='" + token + "'\n"},
		{"ca bundle", false, "", "AIHA_AGENT_FRAMEWORK_URL='" + url + "'\n" +
			"AIHA_AGENT_FRAMEWORK_CERT_PIN='false'\n" +
			"AIHA_AGENT_FRAMEWORK_CERT_SHA256=''\n" +
			"AIHA_AGENT_REGISTRATION_TOKEN='" + token + "'\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			printAgentEnv(&b, url, tt.pin, tt.sha, token)
			if b.String() != tt.want {
				t.Errorf("got\n%s\nwant\n%s", b.String(), tt.want)
			}
		})
	}
}
