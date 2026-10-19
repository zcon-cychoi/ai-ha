package cmd

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/framework/inventory"
	"github.com/spf13/cobra"
)

var agentInventoryCmd = &cobra.Command{
	Use:   "inventory <systemId>",
	Short: "Show what a system is made of, as its agent reported it",
	Long: `Show the partitions a system's agent reported, and whether they add up.

This is what "aiha-framework job backup" chooses from when no --device is
given. A partition that cannot be backed up is listed with the reason, so that
a missing one can be told from an excluded one.

COLLECTED is when the agent read the system; REPORTED is when the framework
heard it. They are separate on purpose: an agent that reports punctually from a
stale reading is a different fault from one that has gone quiet, and only the
first can put a partition that no longer exists into a backup job.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		systemID := args[0]

		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		svc := newInventoryService(pool)
		stored, err := svc.Get(ctx, systemID)
		if errors.Is(err, inventory.ErrNoInventory) {
			fmt.Fprintf(cmd.OutOrStdout(),
				"System %s has not reported what it is made of.\n"+
					"Its agent reports on startup and periodically after that; until then, "+
					"a backup of it has to name its partitions with --device.\n", systemID)
			return nil
		}
		if err != nil {
			return err
		}

		printInventory(cmd, svc, stored)
		return nil
	},
}

// printInventory writes an inventory for an operator.
func printInventory(cmd *cobra.Command, svc *inventory.Service, stored inventory.Stored) {
	out := cmd.OutOrStdout()
	inv := stored.Inventory

	fmt.Fprintf(out, "System:     %s\n", stored.SystemID)
	fmt.Fprintf(out, "Agent:      %s\n", stored.AgentID)
	fmt.Fprintf(out, "Collected:  %s (%s ago)\n",
		stored.CollectedAt.Format(time.RFC3339), stored.Age().Round(time.Second))
	fmt.Fprintf(out, "Reported:   %s\n", stored.ReportedAt.Format(time.RFC3339))
	if platform := describePlatform(inv); platform != "" {
		fmt.Fprintf(out, "Platform:   %s\n", platform)
	}

	if stored.Complete {
		fmt.Fprintln(out, "Complete:   yes — backup targets can be chosen automatically")
	} else {
		fmt.Fprintln(out, "Complete:   no — backup targets will not be chosen automatically")
		if inv.IncompleteReason != "" {
			fmt.Fprintf(out, "            %s\n", inv.IncompleteReason)
		}
		for _, m := range inv.Missing {
			fmt.Fprintf(out, "            missing: %s\n", m)
		}
	}
	if stored.Age() >= svc.StaleAfter() {
		fmt.Fprintln(out, "            this reading is old; the system may have changed since")
	}

	fmt.Fprintln(out)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DEVICE\tMOUNT\tFS\tSIZE\tUSED\tBACKUP\tNOTE")
	for _, p := range inv.Partitions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			orDash(p.Device), orDash(p.MountPoint), orDash(p.FSType),
			describeBytes(p.FSSizeBytes), describeBytes(p.FSUsedBytes),
			describeUse(p), orDash(p.Reason))
	}
	_ = w.Flush()
}

// describeUse says how a partition may be backed up.
func describeUse(p inventory.Partition) string {
	switch {
	case p.Candidate:
		return "automatic"
	case p.Selectable:
		return "by name"
	default:
		return "no"
	}
}

// describeBytes renders a size, distinguishing one that was never determined
// from a filesystem that is empty.
func describeBytes(n *int64) string {
	if n == nil {
		return "-"
	}
	const unit = 1024
	if *n < unit {
		return fmt.Sprintf("%dB", *n)
	}
	div, exp := int64(unit), 0
	for size := *n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(*n)/float64(div), "KMGTPE"[exp])
}

// describePlatform summarises the system for the header line.
func describePlatform(inv inventory.Inventory) string {
	var parts []string
	for _, s := range []string{inv.OSID, inv.OSVersionID, inv.Architecture, inv.Firmware, inv.KernelRelease} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}
