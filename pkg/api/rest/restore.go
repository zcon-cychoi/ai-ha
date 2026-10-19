package rest

import (
	"errors"
	"net/http"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/restore"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
)

// restoreHandler serves the endpoints the rescue agent calls during a restore.
type restoreHandler struct {
	svc *restore.Service
}

// RestPostRestoreClaim godoc
// @ID PostRestoreClaim
// @Summary Claim a restore job
// @Description Called by the agent of an instance booted into rescue mode for a restore. Presenting the job's claim token returns the restore plan and repository access. Retrying with the same request key returns the same response until the redelivery window closes.
// @Tags [Agent] Restore
// @Accept json
// @Produce json
// @Param recoveryId path string true "Recovery ID injected into the rescue instance"
// @Param restoreClaimReq body model.RestoreClaimReq true "Claim token and request key"
// @Success 200 {object} model.RestoreClaimResp
// @Failure 400 {object} model.SimpleMsg
// @Failure 401 {object} model.SimpleMsg
// @Failure 404 {object} model.SimpleMsg
// @Failure 409 {object} model.SimpleMsg
// @Failure 410 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Router /agent/restore/{recoveryId}/claim [post]
func (h *restoreHandler) RestPostRestoreClaim(c echo.Context) error {
	ctx := c.Request().Context()
	recoveryID := c.Param("recoveryId")

	var req model.RestoreClaimReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Malformed request body: check JSON syntax"})
	}
	if err := c.Validate(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Claim token and request key required"})
	}

	resp, err := h.svc.Claim(ctx, recoveryID, req.ClaimToken, req.RequestKey)
	if err != nil {
		status, msg := claimFailure(err)
		evt := log.Warn()
		if status >= http.StatusInternalServerError {
			evt = log.Error()
		}
		evt.Err(err).Str("recoveryId", recoveryID).Int("status", status).Msg("Restore claim refused")
		return c.JSON(status, model.SimpleMsg{Message: msg})
	}

	log.Info().Str("recoveryId", recoveryID).Str("restoreId", resp.RestoreID).Msg("Restore job claimed")
	return c.JSON(http.StatusOK, resp)
}

// claimFailure maps a claim error to the HTTP status and caller-facing message.
func claimFailure(err error) (int, string) {
	switch {
	case errors.Is(err, restore.ErrNotFound):
		return http.StatusNotFound, "Recovery not found"
	case errors.Is(err, restore.ErrInvalidToken):
		return http.StatusUnauthorized, "Claim token rejected"
	case errors.Is(err, restore.ErrClaimedByOther):
		return http.StatusConflict, "Recovery already claimed"
	case errors.Is(err, restore.ErrClaimExpired):
		return http.StatusGone, "Claim window expired"
	case errors.Is(err, restore.ErrRedeliveryExpired):
		return http.StatusGone, "Claim response no longer available"
	case errors.Is(err, restore.ErrLegacyJob):
		return http.StatusConflict, "Restore job names no repository: create it again"
	default:
		return http.StatusInternalServerError, "Claim failed"
	}
}

// RestPostRestoreProgress godoc
// @ID PostRestoreProgress
// @Summary Report progress on a restore
// @Description Called by the rescue agent while it restores. The first report moves a claimed job to restoring. Reports carry an increasing sequence number; one older than the last recorded is accepted and ignored.
// @Tags [Agent] Restore
// @Accept json
// @Produce json
// @Param recoveryId path string true "Recovery ID injected into the rescue instance"
// @Param restoreProgressReq body model.RestoreProgressReq true "Progress report"
// @Success 204 "Progress recorded"
// @Failure 400 {object} model.SimpleMsg
// @Failure 401 {object} model.SimpleMsg
// @Failure 404 {object} model.SimpleMsg
// @Failure 409 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Security RestoreReportToken
// @Router /agent/restore/{recoveryId}/progress [post]
func (h *restoreHandler) RestPostRestoreProgress(c echo.Context) error {
	ctx := c.Request().Context()
	recoveryID := c.Param("recoveryId")

	reportToken, ok := bearerToken(c.Request().Header.Get(echo.HeaderAuthorization))
	if !ok {
		return c.JSON(http.StatusUnauthorized, model.SimpleMsg{Message: "Report token required"})
	}
	var req model.RestoreProgressReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Malformed request body: check JSON syntax"})
	}
	if err := c.Validate(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Sequence and phase required"})
	}

	err := h.svc.RecordProgress(ctx, recoveryID, reportToken, restore.Progress{
		Seq:            req.Seq,
		Phase:          req.Phase,
		Step:           req.Step,
		TotalSteps:     req.TotalSteps,
		Item:           req.Item,
		ProcessedBytes: req.ProcessedBytes,
		TotalBytes:     req.TotalBytes,
	})
	if err != nil {
		return reportFailure(c, err, recoveryID, "Restore progress refused")
	}
	return c.NoContent(http.StatusNoContent)
}

// RestPostRestoreResult godoc
// @ID PostRestoreResult
// @Summary Report how a restore ended
// @Description Called by the rescue agent when the restore ends. A succeeded restore waits for its boot to be confirmed; a failed one ends the job. Repeating the recorded result is accepted, since the agent cannot know whether its first report arrived; a different result is refused.
// @Tags [Agent] Restore
// @Accept json
// @Produce json
// @Param recoveryId path string true "Recovery ID injected into the rescue instance"
// @Param restoreResultReq body model.RestoreResultReq true "Result"
// @Success 204 "Result recorded"
// @Failure 400 {object} model.SimpleMsg
// @Failure 401 {object} model.SimpleMsg
// @Failure 404 {object} model.SimpleMsg
// @Failure 409 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Security RestoreReportToken
// @Router /agent/restore/{recoveryId}/result [post]
func (h *restoreHandler) RestPostRestoreResult(c echo.Context) error {
	ctx := c.Request().Context()
	recoveryID := c.Param("recoveryId")

	reportToken, ok := bearerToken(c.Request().Header.Get(echo.HeaderAuthorization))
	if !ok {
		return c.JSON(http.StatusUnauthorized, model.SimpleMsg{Message: "Report token required"})
	}
	var req model.RestoreResultReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Malformed request body: check JSON syntax"})
	}
	if err := c.Validate(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Outcome must be succeeded or failed"})
	}

	status, err := h.svc.RecordResult(ctx, recoveryID, reportToken, restore.Result{
		Outcome: restore.Outcome(req.Outcome),
		Phase:   req.Phase,
		Message: req.Message,
	})
	if err != nil {
		return reportFailure(c, err, recoveryID, "Restore result refused")
	}
	if status == restore.StatusClaimed {
		log.Warn().Str("recoveryId", recoveryID).Str("phase", req.Phase).
			Msg("Restore failed before changing the disk; it can be run again")
	} else {
		log.Info().Str("recoveryId", recoveryID).Str("outcome", req.Outcome).
			Str("status", string(status)).Msg("Restore finished")
	}
	return c.NoContent(http.StatusNoContent)
}

// reportFailure answers a refused progress or result report. Neither the
// report token nor the request body is logged.
func reportFailure(c echo.Context, err error, recoveryID, logMsg string) error {
	status, msg := reportStatusFor(err)
	evt := log.Warn()
	if status >= http.StatusInternalServerError {
		evt = log.Error()
	}
	evt.Err(err).Str("recoveryId", recoveryID).Int("status", status).Msg(logMsg)
	return c.JSON(status, model.SimpleMsg{Message: msg})
}

func reportStatusFor(err error) (int, string) {
	switch {
	case errors.Is(err, restore.ErrNotFound):
		return http.StatusNotFound, "Recovery not found"
	case errors.Is(err, restore.ErrReportTokenRejected):
		return http.StatusUnauthorized, "Report token rejected"
	case errors.Is(err, restore.ErrRestoreFinished):
		return http.StatusConflict, "Restore already finished"
	case errors.Is(err, restore.ErrResultConflict):
		return http.StatusConflict, "A different result was already recorded for this restore"
	default:
		return http.StatusInternalServerError, "Restore report failed"
	}
}
