//go:build !rescue

package cmd

import (
	"context"
	"reflect"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/rs/zerolog/log"
)

// Bounds on the report interval the framework asks for. The floor keeps a
// misconfigured framework from turning the checks into a loop; the ceiling
// keeps a node from going quiet for so long that it reads as dead.
const (
	minHealthInterval = 10 * time.Second
	maxHealthInterval = 10 * time.Minute
)

// healthReporter is the part of the framework client this loop uses.
type healthReporter interface {
	Health(ctx context.Context, agentToken string, req model.AgentHealthReq) (model.AgentHealthResp, error)
}

// healthMeasurer takes the measurements an assignment asks for. It is a
// parameter so that the loop can be exercised without a service to check.
type healthMeasurer func(ctx context.Context, a *model.HAAssignment) model.AgentHealthReq

// reportHealth reports this node's health for as long as ctx lasts, and keeps
// the HA assignment each answer carries.
//
// It runs beside the job loop, never inside it. The framework judges a node
// silent from these reports alone, and a backup can hold the job loop for
// hours; a primary busy with a backup must not look like one that has died.
//
// It never stops the agent. A system in no HA group reports so that it notices
// when it joins one; it costs one indexed lookup per interval.
func reportHealth(ctx context.Context, client healthReporter, measure healthMeasurer, agentToken string) {
	var assignment *model.HAAssignment
	delay := backoffBase
	// remeasured is set after an immediate report under a new assignment, so
	// that a framework whose answers keep changing cannot drive a loop.
	remeasured := false

	for {
		resp, err := client.Health(ctx, agentToken, measure(ctx, assignment))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn().Err(err).Dur("retryIn", delay).Msg("Could not report this node's health")
			if !sleepOrDone(ctx, jitter(delay)) {
				return
			}
			delay = min(2*delay, maxHealthInterval)
			continue
		}
		delay = backoffBase

		next := resp.Assignment
		if !resp.Assigned {
			next = nil
		}
		changed := !reflect.DeepEqual(assignment, next)
		if changed {
			logAssignment(assignment, next)
		}
		assignment = next

		// Measured at once under a new assignment: the report just sent was
		// taken under the old one, and the framework does not judge it.
		if changed && next != nil && !remeasured {
			remeasured = true
			continue
		}
		remeasured = false

		if !sleepOrDone(ctx, healthInterval(resp.ReportIntervalSeconds)) {
			return
		}
	}
}

// healthInterval bounds the interval the framework asked for.
func healthInterval(seconds int) time.Duration {
	return min(max(time.Duration(seconds)*time.Second, minHealthInterval), maxHealthInterval)
}

// logAssignment records a change of HA assignment. It is rare and worth
// seeing in the agent's own log.
func logAssignment(prev, next *model.HAAssignment) {
	if next == nil {
		log.Info().Str("groupId", prev.GroupID).Msg("This node no longer belongs to an HA group")
		return
	}
	log.Info().Str("groupId", next.GroupID).Str("role", next.Role).
		Str("unit", next.Service.Unit).Str("resource", next.Resource.Name).
		Msg("HA assignment received")
}
