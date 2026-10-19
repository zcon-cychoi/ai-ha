// Package transport builds the HTTP client agents reach the framework with.
//
// A framework with a self-signed certificate, reached through port forwarding,
// has its identity established by pinning rather than by a chain of trust. A
// framework with a publicly trusted certificate is verified the ordinary way,
// against the system's CA bundle and the host name in the URL. Either way, that
// verification and the refusal to follow redirects that would sidestep it are
// the whole of the agent's assurance that it is talking to the framework it
// was told about — which is why they live here once rather than in each caller.
package transport

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrPinMismatch reports that the server presented a certificate other than
	// the pinned one.
	ErrPinMismatch = errors.New("framework certificate does not match the pinned fingerprint")
	// ErrRedirect reports that the framework answered with a redirect, which
	// the agent never follows.
	ErrRedirect = errors.New("framework answered with a redirect")
)

// ParseCertPin reads a certificate-pinning switch. An unset value means on:
// pinning is the stricter check. Anything but true or false is an error rather
// than a guess, because a mistyped value read as "off" would quietly weaken how
// the framework is verified. name is the setting's name, for the message.
func ParseCertPin(value, name string) (bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return true, nil
	}
	pin, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, got %q", name, value)
	}
	return pin, nil
}

// NewClient returns a client that never follows redirects: a 307/308 resends
// the body, which may hold a token, and an http:// target would bypass the
// verification entirely.
//
// With a pin, it completes TLS handshakes only with a server whose leaf
// certificate's DER bytes hash to pin; chain and host name are not checked.
// With a nil pin it verifies the server the standard way, against the system's
// CA bundle and the host name in the URL. The caller chooses between the two
// explicitly: a pin that went missing must never turn into the weaker check.
//
// timeout bounds one request end to end. It is a parameter rather than a
// constant because a long poll has to outlast a short request by design: a
// client deadline shorter than the wait the server was asked to hold would cut
// every poll off before it could answer.
func NewClient(pin []byte, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     newTransport(pin),
		CheckRedirect: func(*http.Request, []*http.Request) error { return ErrRedirect },
	}
}

// newTransport builds the TLS configuration behind NewClient.
func newTransport(pin []byte) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	// ResponseHeaderTimeout is deliberately left unset. A long poll holds the
	// connection open without sending headers for as long as it was asked to
	// wait, so any value here shorter than that wait would break polling
	// without breaking anything else first.
	if pin == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		return transport
	}
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The framework uses a self-signed certificate reached through port
		// forwarding, so chain and hostname checks are replaced by the pin
		// checked in VerifyConnection, which runs even with this set.
		InsecureSkipVerify: true, //nolint:gosec // G402: verified by certificate pinning in VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return ErrPinMismatch
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(sum[:], pin) != 1 {
				return ErrPinMismatch
			}
			return nil
		},
	}
	return transport
}
