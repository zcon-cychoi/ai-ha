package rest

import (
	"errors"
	"net/http"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
)

// identityOf builds the authenticated caller for the job service.
func identityOf(c echo.Context) job.Identity {
	return job.Identity{AgentID: agentIDOf(c), SystemID: systemIDOf(c)}
}

// RestPostAgentJobClaim godoc
// @ID PostAgentJobClaim
// @Summary Claim a job
// @Description Take a job handed out by the long poll. Work must not start until this succeeds: two agents can be handed the same job and only the claim decides between them. Retrying with the same request key before any progress is reported returns the same attempt with a new lease token, since the first response was evidently lost. The response carries the repository the job writes to, credentials included, decrypted for this response; if it cannot be opened the claim is not recorded and may be retried.
// @Tags [Agent] Jobs
// @Accept json
// @Produce json
// @Param jobId path string true "Job ID"
// @Param agentJobClaimReq body model.AgentJobClaimReq true "Claim request"
// @Success 200 {object} model.AgentJobClaimResp
// @Failure 400 {object} model.SimpleMsg
// @Failure 401 {object} model.SimpleMsg
// @Failure 403 {object} model.SimpleMsg
// @Failure 404 {object} model.SimpleMsg
// @Failure 409 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Security AgentToken
// @Router /agent/jobs/{jobId}/claim [post]
func (h *agentHandler) RestPostAgentJobClaim(c echo.Context) error {
	ctx := c.Request().Context()
	jobID := c.Param("jobId")

	var req model.AgentJobClaimReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Malformed request body: check JSON syntax"})
	}
	if err := c.Validate(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Claim request key required"})
	}

	claimed, err := h.jobs.Claim(ctx, identityOf(c), jobID, req.ClaimRequestKey)
	if err != nil {
		return jobFailure(c, err, jobID, "Job claim refused")
	}

	// The lease token is returned here and logged nowhere.
	log.Info().Str("jobId", jobID).Str("attemptId", claimed.Attempt.AttemptID).
		Str("agentId", agentIDOf(c)).Msg("Job claimed")
	return c.JSON(http.StatusOK, model.AgentJobClaimResp{
		JobID:                   claimed.Job.JobID,
		AttemptID:               claimed.Attempt.AttemptID,
		LeaseToken:              claimed.LeaseToken,
		Spec:                    claimed.Job.Spec,
		ProgressIntervalSeconds: int(claimed.Timings.ProgressInterval.Seconds()),
		LeaseSeconds:            int(claimed.Timings.LeaseTTL.Seconds()),
		Repository:              claimed.Repository,
	})
}

// RestPostAgentJobProgress godoc
// @ID PostAgentJobProgress
// @Summary Report progress on a job
// @Description Report on a running attempt and renew its lease. Send this periodically whether or not the work advanced: it is what keeps the lease alive. A cancel request travels back on the response, as there is no other path to an agent.
// @Tags [Agent] Jobs
// @Accept json
// @Produce json
// @Param jobId path string true "Job ID"
// @Param agentJobProgressReq body model.AgentJobProgressReq true "Progress report"
// @Success 200 {object} model.AgentJobProgressResp
// @Failure 400 {object} model.SimpleMsg
// @Failure 401 {object} model.SimpleMsg
// @Failure 403 {object} model.SimpleMsg
// @Failure 404 {object} model.SimpleMsg
// @Failure 409 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Security AgentToken
// @Router /agent/jobs/{jobId}/progress [post]
func (h *agentHandler) RestPostAgentJobProgress(c echo.Context) error {
	ctx := c.Request().Context()
	jobID := c.Param("jobId")

	var req model.AgentJobProgressReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Malformed request body: check JSON syntax"})
	}
	if err := c.Validate(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Attempt ID, lease token and sequence required"})
	}

	reported, err := h.jobs.RecordProgress(ctx, identityOf(c), jobID, req.AttemptID, req.LeaseToken, job.Progress{
		Seq:            req.Seq,
		Phase:          req.Phase,
		ProcessedBytes: req.ProcessedBytes,
		Advanced:       req.Advanced,
	})
	if err != nil {
		return jobFailure(c, err, jobID, "Job progress refused")
	}
	return c.JSON(http.StatusOK, model.AgentJobProgressResp{CancelRequested: reported.CancelRequested})
}

// RestPostAgentJobResult godoc
// @ID PostAgentJobResult
// @Summary Report how a job ended
// @Description Finish an attempt. Repeating the same result is accepted, since the agent cannot know whether its first report arrived; a different result for an attempt already finished is refused.
// @Tags [Agent] Jobs
// @Accept json
// @Produce json
// @Param jobId path string true "Job ID"
// @Param agentJobResultReq body model.AgentJobResultReq true "Result"
// @Success 204 "Result recorded"
// @Failure 400 {object} model.SimpleMsg
// @Failure 401 {object} model.SimpleMsg
// @Failure 403 {object} model.SimpleMsg
// @Failure 404 {object} model.SimpleMsg
// @Failure 409 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Security AgentToken
// @Router /agent/jobs/{jobId}/result [post]
func (h *agentHandler) RestPostAgentJobResult(c echo.Context) error {
	ctx := c.Request().Context()
	jobID := c.Param("jobId")

	var req model.AgentJobResultReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Malformed request body: check JSON syntax"})
	}
	if err := c.Validate(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Attempt ID, lease token and a valid outcome required"})
	}

	finished, err := h.jobs.RecordResult(ctx, identityOf(c), jobID, req.AttemptID, req.LeaseToken, job.Result{
		Outcome: job.Outcome(req.Outcome),
		Message: req.Message,
		Detail:  req.Detail,
	})
	if err != nil {
		return jobFailure(c, err, jobID, "Job result refused")
	}

	log.Info().Str("jobId", jobID).Str("attemptId", req.AttemptID).
		Str("status", string(finished.Status)).Msg("Job finished")
	return c.NoContent(http.StatusNoContent)
}

// jobFailure answers a refused job request.
//
// The statuses tell the agent what to do next. A stale lease (409) means this
// attempt is no longer the current one and must stop; a job owned by another
// system (403) is not something the caller should retry; a store failure (500)
// says nothing about the attempt and may be retried.
func jobFailure(c echo.Context, err error, jobID, logMsg string) error {
	status, msg := jobStatusFor(err)
	evt := log.Warn()
	if status >= http.StatusInternalServerError {
		evt = log.Error()
	}
	// Neither the lease token nor the request body is logged.
	evt.Err(err).Str("jobId", jobID).Str("agentId", agentIDOf(c)).Int("status", status).Msg(logMsg)
	return c.JSON(status, model.SimpleMsg{Message: msg})
}

func jobStatusFor(err error) (int, string) {
	switch {
	case errors.Is(err, job.ErrInvalidRequest):
		return http.StatusBadRequest, "Job request rejected: check the request"
	case errors.Is(err, job.ErrJobNotFound):
		return http.StatusNotFound, "Job not found"
	case errors.Is(err, job.ErrNotOwned):
		return http.StatusForbidden, "Job belongs to another system"
	case errors.Is(err, job.ErrNotClaimable):
		return http.StatusConflict, "Job is not available to claim"
	case errors.Is(err, job.ErrLeaseInvalid):
		return http.StatusConflict, "Lease is no longer valid for this attempt: stop work on it"
	case errors.Is(err, job.ErrResultConflict):
		return http.StatusConflict, "A different result was already recorded for this attempt"
	case errors.Is(err, job.ErrLegacyJob):
		return http.StatusConflict, "Job names no repository: create it again"
	default:
		return http.StatusInternalServerError, "Job request failed"
	}
}
