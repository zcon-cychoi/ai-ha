//go:build !rescue

package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/sysinfo"
	"github.com/rs/zerolog/log"
)

// collectTimeout bounds one reading of the system. Collection runs lsblk and
// sfdisk, which a wedged disk can hold up indefinitely.
const collectTimeout = 2 * time.Minute

// inventoryReporter is the part of the framework client this loop uses.
type inventoryReporter interface {
	Inventory(ctx context.Context, agentToken string, req model.AgentInventoryReq) error
}

// systemReader produces the report for one tick. It is a parameter so that the
// loop's behaviour around a reading that fails can be exercised without a
// system to break.
type systemReader func(ctx context.Context) (model.AgentInventoryReq, error)

// reportInventory tells the framework what this system is made of, so that a
// backup can be asked for without its partitions being named by hand.
//
// It never stops the agent. An agent that cannot describe itself can still run
// the jobs it is given, and refusing to work over it would take a fleet
// offline for a reporting fault.
//
// A failed report is retried with backoff rather than waiting out the whole
// interval: a new agent whose first report fails would otherwise be unable to
// receive an automatically targeted backup until the next tick, which at the
// default setting is an hour away.
func reportInventory(ctx context.Context, client inventoryReporter, read systemReader, agentToken string, every time.Duration) {
	delay := backoffBase
	for {
		if err := reportOnce(ctx, client, read, agentToken); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn().Err(err).Dur("retryIn", delay).Msg("Could not report this system's configuration")
			if !sleepOrDone(ctx, jitter(delay)) {
				return
			}
			// Capped at the normal interval, so that a framework which will
			// never accept a report settles into the ordinary cadence instead
			// of retrying forever at speed.
			delay = min(2*delay, every)
			continue
		}

		delay = backoffBase
		if !sleepOrDone(ctx, every) {
			return
		}
	}
}

// reportOnce reads the system and reports it.
//
// It reports nothing when the mount table could not be read. An inventory
// built from no mounts describes no partitions, and storing it would replace a
// good description with an empty one that looks just as freshly confirmed —
// the framework would then refuse to choose targets for a system that is
// perfectly healthy, and an operator would have no way to tell why.
func reportOnce(ctx context.Context, client inventoryReporter, read systemReader, agentToken string) error {
	req, err := read(ctx)
	if err != nil {
		return err
	}

	if err := client.Inventory(ctx, agentToken, req); err != nil {
		return fmt.Errorf("report this system's configuration: %w", err)
	}

	evt := log.Info()
	if !req.Complete {
		// Worth an operator's attention: the framework will not choose backup
		// targets for this system until it is resolved.
		evt = log.Warn().Strs("missing", req.Missing).Str("reason", req.IncompleteReason)
	}
	evt.Int("partitions", len(req.Partitions)).Bool("complete", req.Complete).
		Msg("Reported this system's configuration")
	return nil
}

// readSystem reads this system and reduces it to a report.
func readSystem(ctx context.Context) (model.AgentInventoryReq, error) {
	collectCtx, cancel := context.WithTimeout(ctx, collectTimeout)
	defer cancel()

	info, err := sysinfo.Collect(collectCtx, nil)
	if err != nil {
		return model.AgentInventoryReq{}, fmt.Errorf("read this system's configuration: %w", err)
	}
	return inventoryOf(info, nil)
}

// inventoryOf reduces collected system facts to what the framework needs in
// order to choose backup targets.
//
// The full facts are not sent. They include /etc/netplan, which may hold
// secrets such as a wireless passphrase; those belong in a backup manifest,
// which the agent controls, and not in the framework's database.
//
// resolve may be nil, in which case device paths are read from sysfs.
func inventoryOf(info *sysinfo.SystemInfo, resolve sysinfo.DeviceResolver) (model.AgentInventoryReq, error) {
	parts, err := sysinfo.Partitions(info, resolve)
	if err != nil {
		return model.AgentInventoryReq{}, fmt.Errorf("list this system's partitions: %w", err)
	}
	complete := sysinfo.CheckCompleteness(info, parts)

	req := model.AgentInventoryReq{
		CollectedAt:      info.CollectedAt,
		Firmware:         string(info.Firmware),
		Architecture:     info.Kernel.Machine,
		KernelRelease:    info.Kernel.Release,
		OSID:             info.OSRelease.ID,
		OSVersionID:      info.OSRelease.VersionID,
		Partitions:       make([]model.InventoryPartition, 0, len(parts)),
		Complete:         complete.OK,
		Missing:          complete.Missing,
		IncompleteReason: complete.Reason,
	}
	for _, p := range parts {
		req.Partitions = append(req.Partitions, model.InventoryPartition{
			Device:      p.Device,
			MountPoint:  p.MountPoint,
			FSType:      p.FSType,
			FSUUID:      p.FSUUID,
			PartUUID:    p.PartUUID,
			Label:       p.Label,
			DevNo:       p.DevNo,
			FSSizeBytes: p.FSSizeBytes,
			FSUsedBytes: p.FSUsedBytes,
			Candidate:   p.Candidate,
			Selectable:  p.Selectable,
			Reason:      p.Reason,
		})
	}
	return req, nil
}
