package repository

import "testing"

func TestParseSMBTarget(t *testing.T) {
	tests := []struct {
		name                          string
		uri                           string
		host, port, user, share, base string
		wantErr                       bool
	}{
		{
			name: "full", uri: "smb://user@nas:4450/backups/aiha/prod",
			host: "nas", port: "4450", user: "user", share: "backups", base: "aiha/prod",
		},
		{
			name: "default port, no user, share only", uri: "smb://nas/share",
			host: "nas", port: "445", user: "", share: "share", base: "",
		},
		{
			name: "trailing slash", uri: "smb://nas/share/",
			host: "nas", port: "445", share: "share", base: "",
		},
		{name: "missing share", uri: "smb://nas", wantErr: true},
		{name: "missing share with slash", uri: "smb://nas/", wantErr: true},
		{name: "missing host", uri: "smb:///share", wantErr: true},
		{name: "wrong scheme", uri: "sftp://nas/share", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port, user, share, base, err := parseSMBTarget(tt.uri)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.uri)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tt.host || port != tt.port || user != tt.user || share != tt.share || base != tt.base {
				t.Fatalf("got (%q,%q,%q,%q,%q), want (%q,%q,%q,%q,%q)",
					host, port, user, share, base, tt.host, tt.port, tt.user, tt.share, tt.base)
			}
		})
	}
}
