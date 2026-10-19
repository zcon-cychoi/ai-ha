package rescue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/agent/transport"
	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/rs/zerolog/log"
)

const (
	// requestTimeout bounds a single request.
	requestTimeout = 30 * time.Second
	// maxResponseBytes bounds how much of a response is read.
	maxResponseBytes = 1 << 20

	defaultBackoffBase = time.Second
	defaultBackoffMax  = 30 * time.Second
)

var (
	// ErrPinMismatch reports that the server presented a certificate other than
	// the pinned one. It is an alias so that callers of this package need not
	// know where the pinning lives.
	ErrPinMismatch = transport.ErrPinMismatch
	// ErrRedirect reports that the framework answered with a redirect, which
	// the agent never follows.
	ErrRedirect = transport.ErrRedirect
	// ErrClaimRefused reports a final refusal from the framework.
	ErrClaimRefused = errors.New("framework refused the claim")
	// ErrReportRefused reports a progress or result report the framework
	// refused for good, e.g. for a superseded report token.
	ErrReportRefused = errors.New("framework refused the report")
)

// claimClient calls the framework's restore endpoints over a pinned TLS
// connection.
type claimClient struct {
	http        *http.Client
	baseURL     string
	backoffBase time.Duration
	backoffMax  time.Duration
}

// newClaimClient returns a client for rec's framework. Zero backoff values
// select the defaults.
func newClaimClient(rec Recovery, backoffBase, backoffMax time.Duration) *claimClient {
	if backoffBase <= 0 {
		backoffBase = defaultBackoffBase
	}
	if backoffMax <= 0 {
		backoffMax = defaultBackoffMax
	}
	return &claimClient{
		http:        transport.NewClient(rec.CertSHA256, requestTimeout),
		baseURL:     rec.FrameworkURL,
		backoffBase: backoffBase,
		backoffMax:  backoffMax,
	}
}

// claim posts the claim until it succeeds, fails for good, or ctx ends. Only
// connection failures, 429 and 5xx are retried, always with the same request
// key, so a retry after a lost response returns the same result.
func (c *claimClient) claim(ctx context.Context, rec Recovery, requestKey string) (model.RestoreClaimResp, error) {
	body, err := json.Marshal(model.RestoreClaimReq{ClaimToken: rec.ClaimToken, RequestKey: requestKey})
	if err != nil {
		return model.RestoreClaimResp{}, fmt.Errorf("encode claim: %w", err)
	}
	endpoint := c.baseURL + "/aiha/agent/restore/" + url.PathEscape(rec.RecoveryID) + "/claim"

	var out model.RestoreClaimResp
	err = c.retry(ctx, "Claim", rec.RecoveryID, func() (bool, error) {
		data, retryable, err := c.post(ctx, endpoint, "", body, ErrClaimRefused)
		if err != nil {
			return retryable, err
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return false, fmt.Errorf("decode claim response: %w", err)
		}
		return false, nil
	})
	if err != nil {
		return model.RestoreClaimResp{}, err
	}
	return out, nil
}

// retry runs attempt until it succeeds, fails for good, or ctx ends, backing
// off with jitter between attempts. attempt reports whether its failure may
// be retried.
func (c *claimClient) retry(ctx context.Context, what, recoveryID string, attempt func() (bool, error)) error {
	delay := c.backoffBase
	for n := 1; ; n++ {
		retryable, err := attempt()
		if err == nil {
			return nil
		}
		if !retryable {
			return err
		}
		wait := delay/2 + rand.N(delay/2+1)
		log.Warn().Err(err).Str("recoveryId", recoveryID).Int("attempt", n).
			Dur("retryIn", wait).Msg(what + " attempt failed, retrying")
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not complete in time: %w", strings.ToLower(what), err)
		case <-time.After(wait):
		}
		delay = min(2*delay, c.backoffMax)
	}
}

// post sends one request and returns the body of a 2xx response. It reports
// whether a failure may be retried; a final refusal wraps refused. bearer, when
// set, is sent as the Authorization header.
func (c *claimClient) post(ctx context.Context, endpoint, bearer string, body []byte, refused error) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, ErrPinMismatch), errors.Is(err, ErrRedirect):
			return nil, false, err
		case ctx.Err() != nil:
			return nil, false, fmt.Errorf("request did not complete in time: %w", err)
		default:
			return nil, true, fmt.Errorf("reach framework: %w", err)
		}
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		// The request may have been recorded; repeating it is safe because
		// every call made through here is idempotent.
		return nil, true, fmt.Errorf("read response: %w", err)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return data, false, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError:
		return nil, true, fmt.Errorf("framework unavailable: %s", statusText(resp.StatusCode, data))
	default:
		return nil, false, fmt.Errorf("%w: %s", refused, statusText(resp.StatusCode, data))
	}
}

// ReportClient sends a restore's progress and result reports to the framework,
// authenticated by the report token the claim issued.
type ReportClient struct {
	c          *claimClient
	recoveryID string
	token      string
}

// NewReportClient returns a client reporting on rec's restore with
// reportToken over the same pinned connection the claim used.
func NewReportClient(rec Recovery, reportToken string) *ReportClient {
	return &ReportClient{c: newClaimClient(rec, 0, 0), recoveryID: rec.RecoveryID, token: reportToken}
}

// Progress sends one progress report. It does not retry: the next report
// supersedes this one.
func (r *ReportClient) Progress(ctx context.Context, req model.RestoreProgressReq) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode progress: %w", err)
	}
	_, _, err = r.c.post(ctx, r.endpoint("progress"), r.token, body, ErrReportRefused)
	return err
}

// Result sends the result, retrying failures that may pass until it is
// recorded, refused, or ctx ends. Repeating a result is safe: the framework
// accepts the same result again.
func (r *ReportClient) Result(ctx context.Context, req model.RestoreResultReq) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	return r.c.retry(ctx, "Result report", r.recoveryID, func() (bool, error) {
		_, retryable, err := r.c.post(ctx, r.endpoint("result"), r.token, body, ErrReportRefused)
		return retryable, err
	})
}

func (r *ReportClient) endpoint(kind string) string {
	return r.c.baseURL + "/aiha/agent/restore/" + url.PathEscape(r.recoveryID) + "/" + kind
}

// statusText describes an HTTP failure with the framework's message when it
// sent one.
func statusText(code int, body []byte) string {
	var msg model.SimpleMsg
	if json.Unmarshal(body, &msg) == nil && msg.Message != "" {
		return fmt.Sprintf("%d %s", code, msg.Message)
	}
	return fmt.Sprintf("%d %s", code, http.StatusText(code))
}
