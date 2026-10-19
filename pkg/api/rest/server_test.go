package rest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

// getReadyz sends a readiness request. The handler reports 503 because only
// Run marks the service ready, which is beside the point here: what matters is
// that the request reached a route rather than being turned away.
func getReadyz(h http.Handler, header, value string) int {
	req := httptest.NewRequest(http.MethodGet, "/aiha/readyz", nil)
	if header != "" {
		req.Header.Set(header, value)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestSwaggerRouteFollowsConfig(t *testing.T) {
	get := func(h http.Handler) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/aiha/swagger.json", nil))
		return rec.Code
	}

	if code := get(NewHandler(Config{})); code != http.StatusNotFound {
		t.Fatalf("status %d: the API document is served without being enabled", code)
	}
	if code := get(NewHandler(Config{EnableSwagger: true})); code != http.StatusOK {
		t.Fatalf("status %d: the API document is not served when enabled", code)
	}
}

// TestRateLimitIgnoresForwardingHeaders guards the pairing of the rate limiter
// with echo.ExtractIPDirect. Echo's default identity falls back to the
// X-Forwarded-For and X-Real-IP headers, so without the direct extractor a
// caller varying either header gets an unlimited budget.
func TestRateLimitIgnoresForwardingHeaders(t *testing.T) {
	for _, header := range []string{echo.HeaderXForwardedFor, echo.HeaderXRealIP} {
		t.Run(header, func(t *testing.T) {
			h := NewHandler(Config{})
			var denied int
			// Enough requests to exhaust the burst, each claiming to come
			// from a different address.
			for i := range rateLimitBurst + 5 {
				code := getReadyz(h, header, fmt.Sprintf("10.0.0.%d", i))
				if code == http.StatusTooManyRequests {
					denied++
				}
			}
			if denied == 0 {
				t.Fatalf("no request was rate limited: a varying %s bypasses the limit", header)
			}
		})
	}
}

func TestRateLimitAllowsTheBurst(t *testing.T) {
	h := NewHandler(Config{})
	for i := range rateLimitBurst {
		if code := getReadyz(h, "", ""); code == http.StatusTooManyRequests {
			t.Fatalf("request %d of a %d burst was rate limited", i+1, rateLimitBurst)
		}
	}
}

func TestRequestIDIsSet(t *testing.T) {
	h := NewHandler(Config{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/aiha/readyz", nil))
	if rec.Header().Get(echo.HeaderXRequestID) == "" {
		t.Fatal("no request ID on the response: the logged requestId stays empty")
	}
}
