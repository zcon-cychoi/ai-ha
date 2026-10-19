package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/framework/inventory"
	"github.com/spf13/cobra"
)

// backupTargets decides which partitions a backup job should capture, and the
// filesystem UUID expected on each.
//
// It is the step between asking for a backup and queueing one. Without it a
// mistyped device produces a job that looks fine and fails once an agent picks
// it up — or, if the path has been reused, a backup of the wrong filesystem
// that reports success. Both are only visible at restore time.
func backupTargets(ctx context.Context, cmd *cobra.Command, inv *inventory.Service) ([]string, map[string]string, error) {
	out := cmd.OutOrStdout()

	if len(jobCreateDevices) == 0 {
		return chosenTargets(ctx, out, inv)
	}
	return namedTargets(ctx, out, inv)
}

// chosenTargets takes the partitions from what the agent reported.
func chosenTargets(ctx context.Context, out io.Writer, inv *inventory.Service) ([]string, map[string]string, error) {
	devices, expected, stored, err := inv.BackupDevices(ctx, jobCreateSystem)
	switch {
	case errors.Is(err, inventory.ErrNoInventory):
		return nil, nil, fmt.Errorf(
			"system %s has not reported what it is made of yet, so its partitions cannot be chosen: "+
				"wait for its agent to report, or name the partitions with --device",
			jobCreateSystem)

	case errors.Is(err, inventory.ErrIncomplete):
		// Refusing is the point. A set chosen from an inventory the agent could
		// not complete would be missing a filesystem the system needs, the
		// backup would succeed, and only the restore would fail.
		return nil, nil, fmt.Errorf(
			"the partitions of system %s cannot be chosen automatically: %w\n"+
				"Back up the partitions you mean with --device once this is understood",
			jobCreateSystem, err)

	case err != nil:
		return nil, nil, err
	}

	fmt.Fprintf(out, "Backing up %d partition(s) reported by the agent:\n", len(devices))
	printChosen(out, stored, devices)
	warnIfStale(out, inv, stored)
	return devices, expected, nil
}

// namedTargets checks the partitions an operator named.
func namedTargets(ctx context.Context, out io.Writer, inv *inventory.Service) ([]string, map[string]string, error) {
	expected, stored, err := inv.ValidateDevices(ctx, jobCreateSystem, jobCreateDevices)
	switch {
	case errors.Is(err, inventory.ErrNoInventory):
		// The agent has not reported, so there is nothing to check against.
		// The job is still queued: naming the partitions by hand was the only
		// way to ask for a backup before inventories existed, and removing it
		// would take away the way out of exactly this situation. The agent
		// checks the devices again before it writes anything.
		fmt.Fprintf(out,
			"System %s has not reported what it is made of, so these partitions were not checked.\n"+
				"Its agent will check them before it starts.\n", jobCreateSystem)
		return jobCreateDevices, nil, nil

	case errors.Is(err, inventory.ErrUnknownDevice), errors.Is(err, inventory.ErrNotSelectable):
		// The hint goes to the output rather than into the error: the error is
		// logged as one field, where a list would arrive as escaped newlines.
		printSelectable(out, stored)
		return nil, nil, err

	case err != nil:
		return nil, nil, err
	}

	warnIfStale(out, inv, stored)
	return jobCreateDevices, expected, nil
}

// printChosen lists the partitions a backup will capture, so that nobody
// queues a job without seeing what it covers.
//
// One line per device, not per row. A filesystem mounted in several places is
// reported once per mount, and printing every row would show three lines under
// a heading that says one — which reads as three filesystems being captured.
func printChosen(out io.Writer, stored inventory.Stored, devices []string) {
	rows := make(map[string]inventory.Partition, len(stored.Inventory.Partitions))
	for _, p := range stored.Inventory.Partitions {
		if existing, ok := rows[p.Device]; ok && existing.Candidate {
			continue
		}
		rows[p.Device] = p
	}
	for _, device := range devices {
		p := rows[device]
		fmt.Fprintf(out, "  %-20s %-16s %s\n", device, p.MountPoint, p.FSType)
	}
	fmt.Fprintf(out, "Reported %s ago, at %s\n",
		stored.Age().Round(time.Second), stored.CollectedAt.Format(time.RFC3339))
}

// printSelectable lists what an operator could have asked for instead.
func printSelectable(out io.Writer, stored inventory.Stored) {
	selectable := stored.Selectable()
	if len(selectable) == 0 {
		fmt.Fprintln(out, "This system reported no partition that can be backed up.")
		return
	}
	fmt.Fprintln(out, "Partitions this system reported that can be backed up:")
	for _, p := range selectable {
		fmt.Fprintf(out, "  %-20s %-16s %s\n", p.Device, p.MountPoint, p.FSType)
	}
}

// warnIfStale says so when the inventory is old, without refusing.
//
// Refusing here would block a backup over a reporting fault, which is the
// wrong trade: the partitions are checked again by the agent immediately
// before it writes, and that check reads the system as it is now.
func warnIfStale(out io.Writer, inv *inventory.Service, stored inventory.Stored) {
	age := stored.Age()
	if age < inv.StaleAfter() {
		return
	}
	fmt.Fprintf(out,
		"Warning: this system last reported %s ago, at %s. Its partitions may have changed since.\n",
		age.Round(time.Minute), stored.CollectedAt.Format(time.RFC3339))
}
