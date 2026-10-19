package transport

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTLSServer(t *testing.T) (*httptest.Server, []byte) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().Raw)
	return srv, sum[:]
}

func TestNewClientPinned(t *testing.T) {
	srv, pin := newTLSServer(t)

	resp, err := NewClient(pin, time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("the pinned certificate was refused: %v", err)
	}
	_ = resp.Body.Close()

	wrong := make([]byte, len(pin))
	_, err = NewClient(wrong, time.Second).Get(srv.URL)
	if !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("a different pin: err = %v, want ErrPinMismatch", err)
	}
}

// With no pin the standard verification applies, and the test server's
// certificate is signed by no CA the system trusts.
func TestNewClientWithoutPinVerifiesStandardWay(t *testing.T) {
	srv, _ := newTLSServer(t)
	if _, err := NewClient(nil, time.Second).Get(srv.URL); err == nil {
		t.Fatal("an untrusted certificate was accepted without a pin")
	}
}

func TestParseCertPin(t *testing.T) {
	for in, want := range map[string]bool{"": true, "true": true, "TRUE": true, "false": false, " false ": false} {
		got, err := ParseCertPin(in, "X")
		if err != nil || got != want {
			t.Errorf("ParseCertPin(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"ture", "no", "off", "2"} {
		if _, err := ParseCertPin(in, "X"); err == nil {
			t.Errorf("ParseCertPin(%q) accepted", in)
		}
	}
}
