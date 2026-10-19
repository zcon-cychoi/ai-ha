package rest

import (
	"errors"
	"net/http"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/inventory"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
)

// RestPostAgentInventory godoc
// @ID PostAgentInventory
// @Summary Report what this system is made of
// @Description Report the partitions that make up the calling agent's system, so that a backup can be asked for without naming them by hand. The report replaces any earlier one: a system keeps one inventory, and its layout history lives in the backup manifests. An agent that could not account for every filesystem the system needs says so, and backup targets are then not chosen automatically. The system is taken from the agent's token, never from the body.
// @Tags [Agent] Inventory
// @Accept json
// @Produce json
// @Param agentInventoryReq body model.AgentInventoryReq true "Inventory report"
// @Success 200 {object} model.SimpleMsg
// @Failure 400 {object} model.SimpleMsg
// @Failure 401 {object} model.SimpleMsg
// @Failure 500 {object} model.SimpleMsg
// @Security AgentToken
// @Router /agent/inventory [post]
func (h *agentHandler) RestPostAgentInventory(c echo.Context) error {
	ctx := c.Request().Context()

	var req model.AgentInventoryReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, model.SimpleMsg{Message: "Malformed request body: check JSON syntax"})
	}
	if err := c.Validate(&req); err != nil {
		return c.JSON(http.StatusBadRequest,
			model.SimpleMsg{Message: "Inventory rejected: a collection time and at least one partition with a mount point are required"})
	}

	// The system is the one this agent's token was issued for. Taking it from
	// the body would let any valid token describe another machine.
	systemID, agentID := systemIDOf(c), agentIDOf(c)

	if err := h.inventory.Report(ctx, systemID, agentID, req); err != nil {
		status, msg := inventoryReportFailure(err)
		evt := log.Warn()
		if status >= http.StatusInternalServerError {
			evt = log.Error()
		}
		evt.Err(err).Str("systemId", systemID).Str("agentId", agentID).Int("status", status).
			Msg("Inventory report refused")
		return c.JSON(status, model.SimpleMsg{Message: msg})
	}

	// The partitions themselves are not logged: the count and the verdict are
	// what an operator reads, and a device list per agent per hour would bury
	// everything else.
	log.Info().Str("systemId", systemID).Str("agentId", agentID).
		Int("partitions", len(req.Partitions)).Bool("complete", req.Complete).
		Msg("Inventory reported")
	return c.JSON(http.StatusOK, model.SimpleMsg{Message: "Inventory recorded"})
}

// inventoryReportFailure maps a report error to the HTTP status and message.
func inventoryReportFailure(err error) (int, string) {
	if errors.Is(err, inventory.ErrInvalidRequest) {
		return http.StatusBadRequest, "Inventory rejected: check the collection time and the partitions reported"
	}
	return http.StatusInternalServerError, "Inventory could not be recorded"
}
