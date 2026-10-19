package secret

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testBox(t *testing.T) *Box {
	t.Helper()
	b, err := NewBox(bytes.Repeat([]byte{7}, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSealOpenRoundTrip(t *testing.T) {
	b := testBox(t)
	plain := []byte(`{"accessKey":"a","secretKey":"s"}`)
	sealed, err := b.Seal(plain, []byte("repo-1"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(sealed, plain) {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := b.Open(sealed, []byte("repo-1"))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("open = %q, %v", got, err)
	}
}

func TestSealUsesAFreshNonce(t *testing.T) {
	b := testBox(t)
	first, _ := b.Seal([]byte("same"), nil)
	second, _ := b.Seal([]byte("same"), nil)
	n := b.aead.NonceSize()
	if bytes.Equal(first[:n], second[:n]) || bytes.Equal(first, second) {
		t.Fatal("two seals of the same plaintext share a nonce")
	}
}

func TestOpenRefusesWithoutPanicking(t *testing.T) {
	b := testBox(t)
	sealed, _ := b.Seal([]byte("secret value"), []byte("repo-1"))
	other, _ := NewBox(bytes.Repeat([]byte{8}, KeySize))

	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1

	tests := []struct {
		name   string
		box    *Box
		sealed []byte
		aad    string
	}{
		{"another row", b, sealed, "repo-2"},
		{"another key", other, sealed, "repo-1"},
		{"tampered", b, tampered, "repo-1"},
		{"truncated", b, sealed[:len(sealed)-3], "repo-1"},
		{"shorter than a nonce", b, sealed[:5], "repo-1"},
		{"empty", b, nil, "repo-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.box.Open(tt.sealed, []byte(tt.aad)); !errors.Is(err, ErrDecrypt) {
				t.Fatalf("err = %v, want ErrDecrypt", err)
			}
		})
	}
}

func TestKeyValidation(t *testing.T) {
	if _, err := NewBox(make([]byte, 16)); err == nil {
		t.Fatal("a 16-byte key was accepted")
	}

	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	_ = os.WriteFile(good, []byte(strings.Repeat("ab", KeySize)+"\n"), 0o600)
	if _, err := LoadKeyFile(good); err != nil {
		t.Fatalf("hex key file refused: %v", err)
	}
	for name, content := range map[string]string{
		"short":   strings.Repeat("ab", 16),
		"not hex": strings.Repeat("zz", KeySize),
	} {
		path := filepath.Join(dir, name)
		_ = os.WriteFile(path, []byte(content), 0o600)
		if _, err := LoadKeyFile(path); err == nil {
			t.Fatalf("%s key file accepted", name)
		}
	}
	if _, err := LoadKeyFile(""); err == nil {
		t.Fatal("no key file accepted")
	}
}
