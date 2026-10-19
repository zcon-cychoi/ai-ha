package token

import (
	"encoding/base64"
	"testing"
)

func TestNewIsUnpredictable(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		tok, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if seen[tok] {
			t.Fatalf("New returned a repeat: %q", tok)
		}
		seen[tok] = true

		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			t.Fatalf("token is not base64url: %v", err)
		}
		if len(raw) != tokenBytes {
			t.Fatalf("token carries %d bytes, want %d", len(raw), tokenBytes)
		}
	}
}

// A token is URL-safe and unpadded so that it survives being pasted into a
// shell assignment, an env file or a header without quoting.
func TestNewIsSafeToPaste(t *testing.T) {
	for range 100 {
		tok, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		for _, c := range tok {
			isSafe := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
				c >= '0' && c <= '9' || c == '-' || c == '_'
			if !isSafe {
				t.Fatalf("token %q contains %q, which needs quoting", tok, c)
			}
		}
	}
}

func TestMatches(t *testing.T) {
	tok, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hash := Hash(tok)

	if !Matches(hash, tok) {
		t.Error("a token did not match its own hash")
	}
	if Matches(hash, tok+"x") {
		t.Error("a different token matched")
	}
	if Matches(hash, "") {
		t.Error("the empty string matched")
	}
	if Matches(nil, tok) {
		t.Error("a nil hash matched")
	}
}

// The digest, not the token, is what the database holds.
func TestHashDoesNotRevealTheToken(t *testing.T) {
	const tok = "a-token-value"
	hash := Hash(tok)

	if len(hash) != 32 {
		t.Fatalf("hash is %d bytes, want 32 (SHA-256)", len(hash))
	}
	if string(hash) == tok {
		t.Fatal("the hash is the token")
	}
	// Deterministic: the same token must find the same row on every lookup.
	if string(Hash(tok)) != string(hash) {
		t.Fatal("Hash is not deterministic")
	}
}
