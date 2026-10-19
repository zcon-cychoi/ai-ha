// Package framework is the agent's client for the framework's southbound API:
// enrolling, and waiting for work.
package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/agent/transport"
	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
)

const (
	// requestTimeout bounds one ordinary request. A long poll does not use it:
	// it is given the wait it asked for plus a margin, because a client
	// deadline shorter than that wait would cut every poll short.
	requestTimeout = 30 * time.Second
	// pollTimeoutMargin is how much longer than the requested wait a long poll
	// is allowed to take, covering the round trip and the server's own margin.
	pollTimeoutMargin = 30 * time.Second
	// maxResponseBytes bounds how much of a response is read.
	maxResponseBytes = 1 << 20
)

var (
	// ErrLeaseLost reports that the framework no longer recognises this
	// attempt: its lease expired, it was superseded, or its token is wrong.
	// The attempt is over from the framework's point of view, and work on it
	// must stop.
	ErrLeaseLost = errors.New("the lease for this attempt is no longer valid")
	// ErrNotClaimable reports a job that another claim already holds.
	ErrNotClaimable = errors.New("the job is not available to claim")
	// ErrNotOwned reports a job that belongs to another system.
	ErrNotOwned = errors.New("the job belongs to another system")

	// ErrPinMismatch reports a server certificate other than the pinned one.
	ErrPinMismatch = transport.ErrPinMismatch
	// ErrRedirect reports a redirect, which the agent never follows.
	ErrRedirect = transport.ErrRedirect

	// ErrRegistrationRefused reports that enrolment cannot succeed as asked:
	// the token is unusable, or this enrolment already completed. Retrying
	// will not help; the operator has to issue a new registration token.
	ErrRegistrationRefused = errors.New("framework refused the registration")

	// ErrUnauthorized reports that the agent's token is not accepted. It means
	// re-enrolment, not a retry.
	ErrUnauthorized = errors.New("agent token rejected")

	// ErrUnavailable reports that the framework could not answer right now.
	// Unlike ErrUnauthorized this says nothing about the credential.
	ErrUnavailable = errors.New("framework unavailable")
)

// Client calls the framework over a pinned TLS connection.
type Client struct {
	baseURL string
	pin     []byte
	// ordinary serves requests that complete promptly. The long poll gets its
	// own client per call, since its deadline depends on the wait.
	ordinary *http.Client
}

// New returns a client for the framework at baseURL, pinned to the SHA-256 of
// its leaf certificate's DER bytes. A nil pin selects standard TLS
// verification against the system's CA bundle instead.
func New(baseURL string, pin []byte) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse the framework URL: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("the framework URL must be https with a host, got %q", baseURL)
	}
	if pin != nil && len(pin) != 32 {
		return nil, fmt.Errorf("the certificate pin must be 32 bytes, got %d", len(pin))
	}
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		pin:      pin,
		ordinary: transport.NewClient(pin, requestTimeout),
	}, nil
}

// Register enrols this agent.
//
// It is called at most once per registration key: a successful enrolment spends
// the registration token, and the agent token it returns cannot be reissued. A
// retry with the same key is therefore refused rather than answered again,
// which is why the caller stores the key before the first attempt.
func (c *Client) Register(ctx context.Context, req model.AgentRegisterReq) (model.AgentRegisterResp, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return model.AgentRegisterResp{}, fmt.Errorf("encode the registration request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/aiha/agent/register", bytes.NewReader(body))
	if err != nil {
		return model.AgentRegisterResp{}, fmt.Errorf("build the registration request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.ordinary.Do(httpReq)
	if err != nil {
		return model.AgentRegisterResp{}, registrationTransportError(err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		// The enrolment may well have been recorded. Saying so matters: the
		// token is spent either way, and the operator has to look rather than
		// simply try again.
		return model.AgentRegisterResp{}, fmt.Errorf("read the registration response, which may have succeeded: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusOK:
		var out model.AgentRegisterResp
		if err := json.Unmarshal(payload, &out); err != nil {
			return model.AgentRegisterResp{}, fmt.Errorf("decode the registration response: %w", err)
		}
		if out.AgentID == "" || out.AgentToken == "" {
			return model.AgentRegisterResp{}, errors.New("the framework returned an incomplete registration")
		}
		return out, nil

	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return model.AgentRegisterResp{}, fmt.Errorf("%w: %s", ErrUnavailable, statusText(resp, payload))

	default:
		return model.AgentRegisterResp{}, fmt.Errorf("%w: %s", ErrRegistrationRefused, statusText(resp, payload))
	}
}

// PollResult is what one long poll came back with.
type PollResult struct {
	// HasWork is false when the wait elapsed with nothing to do. That is the
	// ordinary idle outcome, not a failure, and must not be backed off.
	HasWork bool
	// Job is the work handed out when HasWork is true. Receiving it grants
	// nothing: it must be claimed before any work starts.
	Job model.AgentJob
}

// Poll waits up to wait for work assigned to this agent.
//
// The 204 that ends an idle wait is the normal path. Treating it as an error
// and backing off would make an idle agent poll ever more slowly, and it would
// stop reporting itself alive.
func (c *Client) Poll(ctx context.Context, agentToken string, wait time.Duration) (PollResult, error) {
	endpoint := fmt.Sprintf("%s/aiha/agent/jobs?wait=%d", c.baseURL, int(wait.Seconds()))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return PollResult{}, fmt.Errorf("build the poll request: %w", err)
	}
	// The token travels in a header, never in the query: the framework's
	// request logger records the URI.
	req.Header.Set("Authorization", "Bearer "+agentToken)

	// A client deadline shorter than the wait would cut off every poll, so it
	// is derived from the wait rather than fixed.
	client := transport.NewClient(c.pin, wait+pollTimeoutMargin)

	resp, err := client.Do(req)
	if err != nil {
		return PollResult{}, pollTransportError(err)
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	switch {
	case resp.StatusCode == http.StatusNoContent:
		return PollResult{HasWork: false}, nil

	case resp.StatusCode == http.StatusOK:
		var j model.AgentJob
		if err := json.Unmarshal(payload, &j); err != nil {
			return PollResult{}, fmt.Errorf("decode the job handed out: %w", err)
		}
		if j.JobID == "" {
			return PollResult{}, errors.New("the framework handed out a job with no id")
		}
		return PollResult{HasWork: true, Job: j}, nil

	case resp.StatusCode == http.StatusUnauthorized:
		return PollResult{}, fmt.Errorf("%w: %s", ErrUnauthorized, statusText(resp, payload))

	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return PollResult{}, fmt.Errorf("%w: %s", ErrUnavailable, statusText(resp, payload))

	default:
		return PollResult{}, fmt.Errorf("unexpected response to a poll: %s", statusText(resp, payload))
	}
}

// registrationTransportError classifies a failure to reach the framework
// during enrolment. Pinning and redirect failures are configuration problems
// and never improve on their own.
func registrationTransportError(err error) error {
	switch {
	case errors.Is(err, ErrPinMismatch), errors.Is(err, ErrRedirect):
		return err
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
}

// pollTransportError classifies a failure to reach the framework while polling.
func pollTransportError(err error) error {
	return registrationTransportError(err)
}

// statusText renders a response for an error message, preferring the
// framework's own wording. A token is never part of it.
func statusText(resp *http.Response, body []byte) string {
	var msg model.SimpleMsg
	if err := json.Unmarshal(body, &msg); err == nil && msg.Message != "" {
		return strconv.Itoa(resp.StatusCode) + " " + msg.Message
	}
	return strconv.Itoa(resp.StatusCode) + " " + http.StatusText(resp.StatusCode)
}

// Retryable reports whether an error is worth trying again after a backoff.
//
// It is deliberately narrow. An unauthorized agent and a refused registration
// are settled: retrying them only delays the operator finding out.
func Retryable(err error) bool {
	return errors.Is(err, ErrUnavailable)
}

// Claim takes a job. Work must not start until it succeeds.
func (c *Client) Claim(ctx context.Context, agentToken, jobID, claimRequestKey string) (model.AgentJobClaimResp, error) {
	var out model.AgentJobClaimResp
	err := c.postJSON(ctx, agentToken, "/aiha/agent/jobs/"+url.PathEscape(jobID)+"/claim",
		model.AgentJobClaimReq{ClaimRequestKey: claimRequestKey}, &out)
	if err != nil {
		return model.AgentJobClaimResp{}, err
	}
	if out.AttemptID == "" || out.LeaseToken == "" {
		return model.AgentJobClaimResp{}, errors.New("the framework returned an incomplete claim")
	}
	return out, nil
}

// Progress reports on a running attempt and renews its lease.
func (c *Client) Progress(ctx context.Context, agentToken, jobID string, req model.AgentJobProgressReq) (model.AgentJobProgressResp, error) {
	var out model.AgentJobProgressResp
	err := c.postJSON(ctx, agentToken, "/aiha/agent/jobs/"+url.PathEscape(jobID)+"/progress", req, &out)
	return out, err
}

// Result reports how an attempt ended.
func (c *Client) Result(ctx context.Context, agentToken, jobID string, req model.AgentJobResultReq) error {
	return c.postJSON(ctx, agentToken, "/aiha/agent/jobs/"+url.PathEscape(jobID)+"/result", req, nil)
}

// Inventory reports what this system is made of, so that a backup can be asked
// for without its partitions being named by hand.
//
// The report names no system: the framework files it under the one this
// agent's token was issued for.
func (c *Client) Inventory(ctx context.Context, agentToken string, req model.AgentInventoryReq) error {
	return c.postJSON(ctx, agentToken, "/aiha/agent/inventory", req, nil)
}

// postJSON sends an authenticated request and decodes the reply into out,
// which may be nil for a reply with no body.
//
// It classifies the response so that callers act on meaning rather than on
// status codes. A 409 from these endpoints means the attempt is no longer
// current — the one answer an agent must never retry.
func (c *Client) postJSON(ctx context.Context, agentToken, path string, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode the request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("build the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The agent token travels in a header; the lease token is in the body.
	// Neither is ever part of the URL the framework logs.
	req.Header.Set("Authorization", "Bearer "+agentToken)

	resp, err := c.ordinary.Do(req)
	if err != nil {
		return registrationTransportError(err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%w: read the response: %w", ErrUnavailable, err)
	}

	switch {
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusNoContent:
		if out == nil || len(payload) == 0 {
			return nil
		}
		if err := json.Unmarshal(payload, out); err != nil {
			return fmt.Errorf("decode the response: %w", err)
		}
		return nil

	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", ErrUnauthorized, statusText(resp, payload))

	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrNotOwned, statusText(resp, payload))

	case resp.StatusCode == http.StatusConflict:
		// On a claim this means someone else holds the job; on a report it
		// means this attempt is no longer the current one.
		if path != "" && strings.HasSuffix(path, "/claim") {
			return fmt.Errorf("%w: %s", ErrNotClaimable, statusText(resp, payload))
		}
		return fmt.Errorf("%w: %s", ErrLeaseLost, statusText(resp, payload))

	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
		return fmt.Errorf("%w: %s", ErrUnavailable, statusText(resp, payload))

	default:
		return fmt.Errorf("unexpected response: %s", statusText(resp, payload))
	}
}
