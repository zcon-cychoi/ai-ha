package rest

import (
	"errors"
	"net/http"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
)

// jobLocation is where a job is followed.
func jobLocation(jobID string) string { return "/aiha/jobs/" + jobID }

// RestGetJobs godoc
// @ID GetJobs
// @Summary List jobs
// @Description List jobs, newest first. Restores have their own list at /restores: a rescue agent is not an enrolled system, so they are not in this queue.
// @Tags [Protection] Job Management
// @Produce json
// @Param systemId query string false "Only this system's jobs"
// @Param kind query string false "Only jobs of this kind" Enums(backup)
// @Param status query string false "Only jobs in this status" Enums(queued, claimed, running, cancelling, completed, failed, cancelled)
// @Param from query string false "Only jobs created at or after this time (RFC 3339)"
// @Param to query string false "Only jobs created before this time (RFC 3339)"
// @Param limit query int false "How many to return, 1-200" default(50)
// @Param offset query int false "How many to skip" default(0)
// @Success 200 {object} model.JobListResp
// @Failure 400 {object} model.SimpleMsg
// @Failure 401 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Security AdminToken
// @Router /jobs [get]
func (h *mgmtHandler) RestGetJobs(c echo.Context) error {
	filter, err := jobFilter(c)
	if err != nil {
		return refuseListParams(c, err)
	}
	if kind := job.Kind(c.QueryParam("kind")); kind != "" {
		if kind != job.KindBackup {
			return refuse(c, nil, http.StatusBadRequest, model.CodeInvalidRequest, "Kind must be backup")
		}
		filter.Kind = kind
	}

	jobs, err := h.jobs.List(c.Request().Context(), filter)
	if err != nil {
		return refuse(c, err, http.StatusInternalServerError, model.CodeInternal, "Jobs could not be listed")
	}
	items := make([]model.JobInfo, 0, len(jobs))
	for _, j := range jobs {
		items = append(items, jobInfo(j, nil))
	}
	return c.JSON(http.StatusOK, model.JobListResp{Items: items})
}

// RestGetJob godoc
// @ID GetJob
// @Summary Get a job
// @Description Return a job and the attempt carrying it out. LastSeenAt and LastProgressAt are separate on purpose: an agent that is seen but not progressing is alive and stuck, while one that is not seen has lost contact.
// @Tags [Protection] Job Management
// @Produce json
// @Param jobId path string true "Job ID"
// @Success 200 {object} model.JobInfo
// @Failure 401 {object} model.SimpleMsg
// @Failure 404 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Security AdminToken
// @Router /jobs/{jobId} [get]
func (h *mgmtHandler) RestGetJob(c echo.Context) error {
	j, attempt, err := h.jobs.Get(c.Request().Context(), c.Param("jobId"))
	if errors.Is(err, job.ErrJobNotFound) {
		return refuse(c, nil, http.StatusNotFound, model.CodeNotFound, "Job not found")
	}
	if err != nil {
		return refuse(c, err, http.StatusInternalServerError, model.CodeInternal, "Job could not be read")
	}
	return c.JSON(http.StatusOK, jobInfo(j, attempt))
}

// RestPostJobCancel godoc
// @ID PostJobCancel
// @Summary Ask a job to stop
// @Description A job still queued is cancelled at once (200). A job an agent is running is only marked (202): the agent learns of it on its next report and has to confirm it stopped, and until then the job stays cancelling. Repeating a cancel is safe. A job that already ended any other way is refused, and so is an HA job (409): it ends with its HA operation.
// @Tags [Protection] Job Management
// @Produce json
// @Param jobId path string true "Job ID"
// @Success 200 {object} model.JobInfo "Cancelled"
// @Success 202 {object} model.JobInfo "Waiting for the agent to stop"
// @Failure 401 {object} model.SimpleMsg
// @Failure 404 {object} model.SimpleMsg
// @Failure 409 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Security AdminToken
// @Router /jobs/{jobId}/cancel [post]
func (h *mgmtHandler) RestPostJobCancel(c echo.Context) error {
	j, err := h.jobs.RequestCancel(c.Request().Context(), c.Param("jobId"))
	if errors.Is(err, job.ErrJobNotFound) {
		return refuse(c, nil, http.StatusNotFound, model.CodeNotFound, "Job not found")
	}
	if errors.Is(err, job.ErrNotCancellable) {
		return refuse(c, nil, http.StatusConflict, model.CodeConflict, "HA jobs end with their HA operation: they cannot be cancelled on their own")
	}
	if err != nil {
		return refuse(c, err, http.StatusInternalServerError, model.CodeInternal, "Job could not be cancelled")
	}

	log.Info().Str("jobId", j.JobID).Str("status", string(j.Status)).
		Str("requestedBy", principalOf(c).Subject).Msg("Job cancel requested")
	switch {
	case j.Status == job.StatusCancelled:
		return c.JSON(http.StatusOK, jobInfo(j, nil))
	case j.Status.Terminal():
		return refuse(c, nil, http.StatusConflict, model.CodeJobFinished, "Job already finished")
	default:
		return c.JSON(http.StatusAccepted, jobInfo(j, nil))
	}
}

// jobFilter reads the query parameters the job and backup lists share.
func jobFilter(c echo.Context) (job.ListFilter, error) {
	p, err := parseListParams(c)
	if err != nil {
		return job.ListFilter{}, err
	}
	status := job.Status(c.QueryParam("status"))
	if status != "" && !status.Known() {
		return job.ListFilter{}, listParamsError("Status is not a job status")
	}
	return job.ListFilter{
		SystemID: c.QueryParam("systemId"),
		Status:   status,
		From:     p.from,
		To:       p.to,
		Limit:    p.limit,
		Offset:   p.offset,
	}, nil
}

func jobInfo(j job.Job, a *job.Attempt) model.JobInfo {
	info := model.JobInfo{
		JobID:           j.JobID,
		SystemID:        j.SystemID,
		Kind:            string(j.Kind),
		Status:          string(j.Status),
		CancelRequested: j.CancelRequested,
		Spec:            j.Spec,
		Message:         j.Message,
		RequestedBy:     j.RequestedBy,
		CreatedAt:       j.CreatedAt,
		ExpiresAt:       j.ExpiresAt,
		StartedAt:       j.StartedAt,
		FinishedAt:      j.FinishedAt,
	}
	if a != nil {
		info.Attempt = &model.JobAttemptInfo{
			AttemptID:      a.AttemptID,
			AgentID:        a.AgentID,
			Phase:          a.Phase,
			ProcessedBytes: a.ProcessedBytes,
			TotalUnknown:   true,
			LastSeenAt:     a.LastSeenAt,
			LastProgressAt: a.LastProgressAt,
			LeaseExpiresAt: a.LeaseExpiresAt,
			Outcome:        string(a.Outcome),
			Message:        a.Message,
			Result:         a.Result,
			FinishedAt:     a.FinishedAt,
		}
	}
	return info
}
