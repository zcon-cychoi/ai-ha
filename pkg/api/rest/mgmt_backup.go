package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/agent"
	"github.com/cloud-barista/ai-ha/pkg/framework/inventory"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/cloud-barista/ai-ha/pkg/framework/repository"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
)

// RestPostSystemBackup godoc
// @ID PostSystemBackup
// @Summary Request a backup of a system
// @Description Queue a backup job for the system's agent and return at once: follow the job at the address in the Location header. With no devices the partitions are chosen from what the agent reported; a system whose report is missing or incomplete is refused, because a set chosen from it could leave out a filesystem the system needs, and only the restore would show it. Named devices are checked against the same report. If there is no report yet they are accepted unchecked, with a warning, and the agent checks them before it writes. A system taking part in an HA operation is refused (409) until the operation ends. Requests are not deduplicated yet: repeating one queues another job.
// @Tags [Protection] Backup Management
// @Accept json
// @Produce json
// @Param systemId path string true "System ID"
// @Param backupReq body model.BackupReq true "Backup request"
// @Success 202 {object} model.BackupAcceptedResp
// @Header 202 {string} Location "Where to follow the job"
// @Failure 400 {object} model.SimpleMsg
// @Failure 401 {object} model.SimpleMsg
// @Failure 404 {object} model.SimpleMsg
// @Failure 409 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Security AdminToken
// @Router /systems/{systemId}/backups [post]
func (h *mgmtHandler) RestPostSystemBackup(c echo.Context) error {
	ctx := c.Request().Context()
	systemID := c.Param("systemId")

	var req model.BackupReq
	if err := c.Bind(&req); err != nil {
		return refuseMalformed(c)
	}
	if err := c.Validate(&req); err != nil {
		return refuse(c, nil, http.StatusBadRequest, model.CodeInvalidRequest,
			"Repository required; devices must be distinct and compression zstd or none")
	}

	system, err := h.agents.GetSystem(ctx, systemID)
	if errors.Is(err, agent.ErrSystemNotFound) {
		return refuse(c, nil, http.StatusNotFound, model.CodeNotFound, "System not found")
	}
	if err != nil {
		return refuse(c, err, http.StatusInternalServerError, model.CodeInternal, "System could not be read")
	}
	// A job for a system with no working agent would sit in the queue until it
	// expired, reading as a backup on its way.
	if system.Agent.RevokedAt != nil {
		return refuse(c, nil, http.StatusConflict, model.CodeConflict, "System has no active agent: enrol one first")
	}

	devices, expected, warnings, err := h.backupTargets(ctx, systemID, req.Devices)
	if err != nil {
		return refuseBackupTargets(c, err)
	}

	created, err := h.jobs.CreateBackup(ctx, systemID, job.BackupSpec{
		Devices:         devices,
		Compression:     req.Compression,
		ExpectedFSUUIDs: expected,
		RepositoryID:    req.RepositoryID,
	}, principalOf(c).Subject)
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return refuse(c, nil, http.StatusBadRequest, model.CodeRepositoryNotFound, "Repository not found")
	case errors.Is(err, job.ErrInvalidRequest):
		return refuse(c, err, http.StatusBadRequest, model.CodeInvalidRequest, "Backup request rejected: check the devices and repository")
	case errors.Is(err, job.ErrSystemInFlow):
		return refuse(c, err, http.StatusConflict, model.CodeConflict, "System is in an HA operation: back it up once the operation ends")
	case err != nil:
		return refuse(c, err, http.StatusInternalServerError, model.CodeInternal, "Backup could not be queued")
	}

	log.Info().Str("jobId", created.JobID).Str("systemId", systemID).
		Str("requestedBy", created.RequestedBy).Msg("Backup job queued")
	c.Response().Header().Set(echo.HeaderLocation, jobLocation(created.JobID))
	return c.JSON(http.StatusAccepted, model.BackupAcceptedResp{
		JobID:     created.JobID,
		SystemID:  created.SystemID,
		Status:    string(created.Status),
		Devices:   devices,
		ExpiresAt: created.ExpiresAt,
		Warnings:  warnings,
	})
}

// backupTargets decides what a backup captures, as protocol.md 2.7 requires of
// every path that creates one. The rules are in the inventory service; what is
// settled here is which of them applies.
func (h *mgmtHandler) backupTargets(ctx context.Context, systemID string, named []string) ([]string, map[string]string, []string, error) {
	if len(named) == 0 {
		devices, expected, stored, err := h.inventory.BackupDevices(ctx, systemID)
		if err != nil {
			return nil, nil, nil, err
		}
		return devices, expected, h.staleWarning(stored), nil
	}

	expected, stored, err := h.inventory.ValidateDevices(ctx, systemID, named)
	if errors.Is(err, inventory.ErrNoInventory) {
		// The stated exception in protocol.md 2.7: with no report to check
		// against, naming partitions is the only way to get a backup at all,
		// and the agent checks them against the real system before it writes.
		return named, nil, []string{"System has not reported its partitions, so these were not checked: its agent checks them before it starts"}, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	return named, expected, h.staleWarning(stored), nil
}

// staleWarning says so when the inventory is old, without refusing: the agent
// re-checks the devices immediately before it writes.
func (h *mgmtHandler) staleWarning(stored inventory.Stored) []string {
	age := stored.Age()
	if age < h.inventory.StaleAfter() {
		return nil
	}
	return []string{fmt.Sprintf("System last reported its partitions %s ago: they may have changed since",
		age.Round(time.Minute))}
}

func refuseBackupTargets(c echo.Context, err error) error {
	switch {
	case errors.Is(err, inventory.ErrNoInventory):
		return refuse(c, nil, http.StatusConflict, model.CodeNoInventory,
			"System has not reported its partitions: name them in devices, or retry once its agent reports")
	case errors.Is(err, inventory.ErrIncomplete):
		return refuse(c, err, http.StatusConflict, model.CodeInventoryIncomplete,
			"System inventory is incomplete, so its partitions cannot be chosen automatically: see its inventory and name them in devices")
	case errors.Is(err, inventory.ErrUnknownDevice), errors.Is(err, inventory.ErrNotSelectable):
		return refuse(c, err, http.StatusBadRequest, model.CodeDeviceNotBackupable,
			"A named device cannot be backed up on this system: see its inventory")
	default:
		return refuse(c, err, http.StatusInternalServerError, model.CodeInternal, "Backup targets could not be determined")
	}
}

// RestGetBackups godoc
// @ID GetBackups
// @Summary List the backup history
// @Description List backups across all systems, newest first, including those that failed or have not run yet. A backup that failed before producing a set has no backup set ID.
// @Tags [Protection] Backup Management
// @Produce json
// @Param systemId query string false "Only this system's backups"
// @Param status query string false "Only backups in this job status" Enums(queued, claimed, running, cancelling, completed, failed, cancelled)
// @Param from query string false "Only backups requested at or after this time (RFC 3339)"
// @Param to query string false "Only backups requested before this time (RFC 3339)"
// @Param limit query int false "How many to return, 1-200" default(50)
// @Param offset query int false "How many to skip" default(0)
// @Success 200 {object} model.BackupListResp
// @Failure 400 {object} model.SimpleMsg
// @Failure 401 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Security AdminToken
// @Router /backups [get]
func (h *mgmtHandler) RestGetBackups(c echo.Context) error {
	filter, err := jobFilter(c)
	if err != nil {
		return refuseListParams(c, err)
	}
	records, err := h.jobs.ListBackups(c.Request().Context(), filter)
	if err != nil {
		return refuse(c, err, http.StatusInternalServerError, model.CodeInternal, "Backups could not be listed")
	}
	items := make([]model.BackupInfo, 0, len(records))
	for _, r := range records {
		items = append(items, backupInfo(r))
	}
	return c.JSON(http.StatusOK, model.BackupListResp{Items: items})
}

// RestGetBackup godoc
// @ID GetBackup
// @Summary Get a backup by its set ID
// @Description Return what became of a backup set. This is the framework's record of what the agent reported: whether the files are still in the repository is not known here, and sizes are per set because the framework does not read manifests.
// @Tags [Protection] Backup Management
// @Produce json
// @Param backupSetId path string true "Backup set ID"
// @Success 200 {object} model.BackupInfo
// @Failure 401 {object} model.SimpleMsg
// @Failure 404 {object} model.SimpleMsg
// @Failure 409 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Security AdminToken
// @Router /backups/{backupSetId} [get]
func (h *mgmtHandler) RestGetBackup(c echo.Context) error {
	record, err := h.jobs.GetBackup(c.Request().Context(), c.Param("backupSetId"))
	switch {
	case errors.Is(err, job.ErrBackupSetNotFound):
		return refuse(c, nil, http.StatusNotFound, model.CodeNotFound, "Backup set not found")
	case errors.Is(err, job.ErrBackupSetAmbiguous):
		return refuse(c, err, http.StatusConflict, model.CodeBackupSetAmbiguous, "More than one backup reported this set")
	case err != nil:
		return refuse(c, err, http.StatusInternalServerError, model.CodeInternal, "Backup could not be read")
	}
	return c.JSON(http.StatusOK, backupInfo(record))
}

// RestDeleteBackup godoc
// @ID DeleteBackup
// @Summary Delete a backup set
// @Description Not implemented yet: answers 501. Backup data never passes through the framework, so deleting a set needs a job an agent carries out, which does not exist.
// @Tags [Protection] Backup Management
// @Produce json
// @Param backupSetId path string true "Backup set ID"
// @Success 204 "Backup set deleted"
// @Failure 401 {object} model.SimpleMsg
// @Failure 501 {object} model.SimpleMsg
// @Security AdminToken
// @Router /backups/{backupSetId} [delete]
func RestDeleteBackup(c echo.Context) error {
	return notImplemented(c)
}

// backupInfo describes one backup. The spec and result are decoded leniently:
// they are history, and one row in an unexpected shape must not cost a caller
// the rest of the list.
func backupInfo(r job.BackupRecord) model.BackupInfo {
	j := r.Job
	info := model.BackupInfo{
		JobID:       j.JobID,
		SystemID:    j.SystemID,
		Status:      string(j.Status),
		Devices:     []string{},
		Message:     j.Message,
		RequestedBy: j.RequestedBy,
		CreatedAt:   j.CreatedAt,
		StartedAt:   j.StartedAt,
		FinishedAt:  j.FinishedAt,
	}

	var spec job.BackupSpec
	if err := json.Unmarshal(j.Spec, &spec); err != nil {
		log.Warn().Err(err).Str("jobId", j.JobID).Msg("Backup job has an unreadable spec")
	} else {
		info.RepositoryID = spec.RepositoryID
		info.Compression = spec.Compression
		if spec.Devices != nil {
			info.Devices = spec.Devices
		}
	}

	if r.Attempt == nil {
		return info
	}
	info.ProcessedBytes = r.Attempt.ProcessedBytes
	if len(r.Attempt.Result) == 0 {
		return info
	}
	var result model.BackupJobResult
	if err := json.Unmarshal(r.Attempt.Result, &result); err != nil {
		log.Warn().Err(err).Str("jobId", j.JobID).Msg("Backup attempt has an unreadable result")
		return info
	}
	info.BackupSetID = result.BackupSetID
	info.SetStatus = result.Status
	info.Items = result.Items
	info.Manifest = result.Manifest
	return info
}
