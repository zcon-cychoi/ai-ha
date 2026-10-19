package rest

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/agent"
	"github.com/cloud-barista/ai-ha/pkg/framework/inventory"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
)

// Long-poll bounds. The lower bound is what stops a caller from spinning: a
// successful poll can return no sooner than this, so an authenticated agent
// cannot turn the endpoint into a busy loop by asking for no wait at all.
const (
	minPollWait = 5 * time.Second
	maxPollWait = 45 * time.Second
	// writeDeadlineMargin leaves room to write the response after the wait.
	writeDeadlineMargin = 10 * time.Second
)

// queueCheckInterval is how often a waiting long poll looks for new work.
//
// It sets how long a newly created job can sit before its agent sees it. A
// backup that starts a few seconds late costs nothing, and checking more often
// multiplies an indexed query by every idle agent. LISTEN/NOTIFY would remove
// the latency without the load, and is the step to take when fleets grow.
const queueCheckInterval = 3 * time.Second

// agentHandler serves the endpoints an enrolled agent calls.
type agentHandler struct {
	svc *agent.Service
	// jobs is nil when the server runs without a queue; the long poll then
	// only keeps agents marked alive.
	jobs *job.Service
	// inventory is nil when the server runs without one; agents then cannot
	// report what their systems are made of, and backup targets have to be
	// named by hand.
	inventory *inventory.Service
}

// RestPostAgentRegister godoc
// @ID PostAgentRegister
// @Summary Enrol an agent
// @Description Spend a registration token to receive an agent identity and its long-lived token. The token is returned only here. Retrying with the same request key reports that the enrolment already completed.
// @Tags [Agent] Enrolment
// @Accept json
// @Produce json
// @Param agentRegisterReq body model.AgentRegisterReq true "Registration request"
// @Success 200 {object} model.AgentRegisterResp
// @Failure 400 {object} model.SimpleMsg
// @Failure 401 {object} model.SimpleMsg
// @Failure 409 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Router /agent/register [post]
func (h *agentHandler) RestPostAgentRegister(c echo.Context) error {
	ctx := c.Request().Context()

	var req model.AgentRegisterReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Malformed request body: check JSON syntax"})
	}
	if err := c.Validate(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Registration token, request key and system ID required"})
	}

	registered, err := h.svc.Register(ctx, req.RegistrationToken, agent.Registration{
		SystemID:        req.SystemID,
		RegistrationKey: req.RegistrationKey,
		Hostname:        req.Hostname,
		AgentVersion:    req.AgentVersion,
	})
	if err != nil {
		status, msg := registerFailure(err)
		evt := log.Warn()
		if status >= http.StatusInternalServerError {
			evt = log.Error()
		}
		// The registration token is never logged, here or anywhere.
		evt.Err(err).Str("systemId", req.SystemID).Int("status", status).
			Msg("Agent registration refused")
		return c.JSON(status, model.SimpleMsg{Message: msg})
	}

	log.Info().Str("agentId", registered.AgentID).Str("systemId", registered.SystemID).
		Msg("Agent registered")
	return c.JSON(http.StatusOK, model.AgentRegisterResp{
		AgentID:    registered.AgentID,
		SystemID:   registered.SystemID,
		AgentToken: registered.Token,
	})
}

// registerFailure maps an enrolment error to the HTTP status and message.
//
// A rejected token and a completed registration are deliberately different:
// the first means "get a token", the second means "you already have an agent;
// its token cannot be reissued, so discard it and enrol again".
func registerFailure(err error) (int, string) {
	switch {
	case errors.Is(err, agent.ErrInvalidRequest):
		return http.StatusBadRequest, "Registration request rejected: check the system ID and request key"
	case errors.Is(err, agent.ErrTokenRejected):
		return http.StatusUnauthorized, "Registration token rejected: it is unknown, expired, used up or revoked"
	case errors.Is(err, agent.ErrAlreadyRegistered):
		return http.StatusConflict, "Registration already completed for this request key: reset the agent and use a new registration token"
	default:
		return http.StatusInternalServerError, "Registration failed"
	}
}

// RestGetAgentJobs godoc
// @ID GetAgentJobs
// @Summary Wait for work
// @Description Long-poll for work assigned to the calling agent's system. Returns the job as soon as one is waiting, or 204 when the wait elapses with nothing to do; the 204 is the normal idle path and must not be treated as an error or backed off. Receiving a job grants nothing: the agent must claim it and see the claim succeed before starting. Reaching this endpoint is also what marks the agent as alive.
// @Tags [Agent] Jobs
// @Produce json
// @Param wait query int false "Seconds to wait for work, clamped to 5-45" default(45)
// @Success 200 {object} model.AgentJob
// @Success 204 "No work is waiting"
// @Failure 401 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Failure 503 {object} model.SimpleMsg
// @Security AgentToken
// @Router /agent/jobs [get]
func (h *agentHandler) RestGetAgentJobs(c echo.Context) error {
	ctx := c.Request().Context()
	agentID := agentIDOf(c)

	// Contact is recorded before the wait, so that liveness does not depend on
	// the poll running to completion.
	if err := h.svc.Touch(ctx, agentID); err != nil {
		log.Error().Err(err).Str("agentId", agentID).Msg("Failed to record agent contact")
		return c.JSON(http.StatusInternalServerError, model.SimpleMsg{Message: "Could not record contact"})
	}

	wait := pollWait(c.QueryParam("wait"))

	if err := extendWriteDeadline(c, wait+writeDeadlineMargin); err != nil {
		return err
	}

	deadline := time.NewTimer(wait)
	defer deadline.Stop()

	check := time.NewTicker(queueCheckInterval)
	defer check.Stop()

	systemID := systemIDOf(c)
	for {
		// Checked before the first wait, so that work already waiting is
		// handed out at once rather than after a tick.
		if h.jobs != nil {
			waiting, err := h.jobs.Waiting(ctx, systemID)
			switch {
			case err == nil:
				// Handed out, not taken: nothing changes until the agent claims
				// it, so a response that never arrives costs nothing.
				return c.JSON(http.StatusOK, model.AgentJob{
					JobID: waiting.JobID,
					Kind:  string(waiting.Kind),
					Spec:  waiting.Spec,
				})
			case errors.Is(err, job.ErrJobNotFound):
			default:
				log.Error().Err(err).Str("agentId", agentID).Msg("Failed to look for waiting work")
				return c.JSON(http.StatusServiceUnavailable,
					model.SimpleMsg{Message: "Service temporarily unavailable, retry shortly"})
			}
		}

		select {
		case <-ctx.Done():
			// The agent hung up or is shutting down. The write goes nowhere, but
			// recording the same status keeps the log honest: nothing was handed
			// out, and a 200 here would suggest otherwise.
			return c.NoContent(http.StatusNoContent)
		case <-deadline.C:
			return c.NoContent(http.StatusNoContent)
		case <-check.C:
		}
	}
}

// extendWriteDeadline gives this request longer to answer than the server's
// own write timeout allows.
//
// That timeout is deliberately short, to shed a caller that opens a connection
// and then stalls. A long poll is the one route that must outlast it, so the
// deadline is lifted for this request alone rather than relaxed for every
// route. The wait is never shortened to fit the timeout instead: an agent
// quietly polling faster than it was told to would hide the cause.
//
// A response writer with no deadline support is not that situation. It has no
// connection to time out — an httptest recorder, for instance — so there is
// nothing to extend and nothing to lose by continuing.
func extendWriteDeadline(c echo.Context, d time.Duration) error {
	err := http.NewResponseController(c.Response()).SetWriteDeadline(time.Now().Add(d))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, http.ErrNotSupported):
		log.Debug().Msg("The response writer has no write deadline to extend")
		return nil
	default:
		return fmt.Errorf("extend the write deadline for a long poll: %w", err)
	}
}

// pollWait parses the requested wait and clamps it.
//
// Clamping the lower bound is what keeps the endpoint from being spun: an
// agent asking for no wait still gets one, so a successful poll cannot repeat
// faster than minPollWait. An unparsable value takes the default rather than
// failing the request, because the wait is a hint, not a instruction.
func pollWait(raw string) time.Duration {
	if raw == "" {
		return maxPollWait
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		return maxPollWait
	}
	wait := time.Duration(seconds) * time.Second
	return min(max(wait, minPollWait), maxPollWait)
}
