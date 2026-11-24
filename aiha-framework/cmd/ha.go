package cmd

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/ha"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

var (
	haCreateSpec       ha.GroupSpec
	haCreateMaxLatency time.Duration
	haEventsAfter      int64
	haEventsLimit      int
)

var haCmd = &cobra.Command{
	Use:   "ha",
	Short: "Manage HA (redundancy) groups",
	Long: `An HA group pairs a primary and a standby system around one replicated data
volume and the service that runs on it (TM-AI-HA-02).

Both systems' agents report their health; the framework judges the pair as ok,
attention or error with the reasons, and records every change as an event of
the group. Registering a group configures nothing on the nodes: it tells their
agents what to check.`,
}

var haCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Register an HA group",
	Long: `Register an HA group of two enrolled systems.

Nothing is configured on the nodes. Their agents learn their roles and what to
check from the answer to their next health report, and the group stays
"registered" until replication is set up.

--primary-disk and --standby-disk are the nodes' data disks, for a group whose
replication "ha setup" configures. Give each as the /dev/disk/by-id path the
system's inventory lists ("aiha-framework system inventory <systemId>"): a
kernel name such as /dev/sdb can name another disk after a reboot. Setup
formats the primary's disk, so it must be a new, empty one.

--resource instead adopts a replication resource that already exists, such as
one set up by hand. Such a group is monitored but not set up: give either the
disks or --resource, not both.

--address values are the nodes' private IP addresses: the standby probes the
primary's service there, and replication uses them.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		svc, err := newHAService(pool)
		if err != nil {
			return err
		}
		spec := haCreateSpec
		spec.Service.MaxHealthLatencyMs = int(haCreateMaxLatency / time.Millisecond)

		gs, err := svc.Create(ctx, spec, "cli")
		if err != nil {
			return err
		}
		log.Info().Str("groupId", gs.GroupID).Str("name", gs.Name).
			Str("primarySystemId", gs.Primary.SystemID).Str("standbySystemId", gs.Standby.SystemID).
			Msg("HA group registered")
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "Registered HA group %s (%s): primary %s, standby %s, resource %s\n",
			gs.Name, gs.GroupID, gs.Primary.SystemID, gs.Standby.SystemID, gs.Resource.Name)
		fmt.Fprintf(out, "Its agents start checking on their next health report, within %s.\n"+
			"Follow it with \"aiha-framework ha status %s\".\n",
			svc.Timings().ReportInterval, gs.Name)
		if gs.Primary.Disk != "" {
			fmt.Fprintf(out, "Set its replication up with \"aiha-framework ha setup %s\" once both nodes report.\n", gs.Name)
		}
		return nil
	},
}

var haSetupCmd = &cobra.Command{
	Use:   "setup <group>",
	Short: "Set up an HA group's replication",
	Long: `Set up replication on a registered HA group, without logging in to its nodes.

The standby's side is configured first. Once its agent reports the resource up
as Secondary, the primary's side is configured: it is promoted, its data disk
is formatted and mounted, and the service is started there. The group is
"configuring" until then and "syncing" while the standby catches up; it becomes
"protected" when the two are in sync and the service is healthy.

Each node does only what is not already so: a disk that already carries
replication metadata is not initialized again, and a volume that already holds
a filesystem is not formatted again. A setup that failed can be asked for again
once its error is acknowledged ("ha acknowledge").

The nodes' service is disabled from starting at boot and made to require the
replicated mount, so that it cannot write to the root filesystem's mount
point on a node that is not primary. It must be stopped when setup starts.

Requires AIHA_FRAMEWORK_SECRET_KEY_FILE: the peers' shared secret is made here
and kept encrypted.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return startHAFlow(cmd, args[0], ha.FlowSetup)
	},
}

var haTeardownCmd = &cobra.Command{
	Use:   "teardown <group>",
	Short: "Remove an HA group's replication",
	Long: `Remove replication from an HA group: on the primary, stop the service,
unmount the volume, step down and take the resource down; then the same on the
standby. The resource file and the units setup wrote are removed. The data and
the replication metadata stay on the disks: erasing them is a manual step
(plan/ha-h0.md section 10). The group returns to "registered".

The service is left stopped and disabled.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return startHAFlow(cmd, args[0], ha.FlowTeardown)
	},
}

var haAcknowledgeCmd = &cobra.Command{
	Use:   "acknowledge <group>",
	Short: "Clear an HA group's error after checking its nodes",
	Long: `Clear an HA group's error.

It is refused while an HA job of the group has not ended, or while the standby
reports itself primary. A group whose last setup or teardown failed returns to
"registered", from where either can be asked for again; any other error, such
as two primaries, returns the group to "degraded" for the evaluator to judge.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		flows, err := newFlowService(pool, nil)
		if err != nil {
			return err
		}
		gs, err := flows.Acknowledge(ctx, args[0], "cli")
		if err != nil {
			return err
		}
		log.Info().Str("groupId", gs.GroupID).Str("state", string(gs.State)).Msg("HA group error acknowledged")
		fmt.Fprintf(cmd.OutOrStdout(), "Acknowledged: %s (%s) is now %s\n", gs.Name, gs.GroupID, gs.State)
		return nil
	},
}

// startHAFlow starts a setup or a teardown and says what happens next.
func startHAFlow(cmd *cobra.Command, group string, kind ha.FlowKind) error {
	ctx := cmd.Context()
	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Only a setup seals a secret; a teardown needs no key.
	var flows *ha.FlowService
	if kind == ha.FlowSetup {
		box, err := loadSecretBox("the HA groups' shared secrets")
		if err != nil {
			return err
		}
		flows, err = newFlowService(pool, box)
		if err != nil {
			return err
		}
	} else if flows, err = newFlowService(pool, nil); err != nil {
		return err
	}

	start := flows.Setup
	if kind == ha.FlowTeardown {
		start = flows.Teardown
	}
	gs, flow, err := start(ctx, group, "cli")
	if err != nil {
		return err
	}
	log.Info().Str("groupId", gs.GroupID).Str("flowId", flow.FlowID).Str("kind", string(flow.Kind)).
		Msg("HA operation started")
	out := cmd.OutOrStdout()
	first := "standby"
	if kind == ha.FlowTeardown {
		first = "primary"
	}
	fmt.Fprintf(out, "Started %s %s on %s (%s); the %s goes first (job %s).\n",
		flow.Kind, flow.FlowID, gs.Name, gs.GroupID, first, flow.Jobs[0].JobID)
	fmt.Fprintf(out, "Follow it with \"aiha-framework ha status %s\".\n", gs.Name)
	return nil
}

var haListCmd = &cobra.Command{
	Use:   "list",
	Short: "List HA groups",
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		svc, err := newHAService(pool)
		if err != nil {
			return err
		}
		groups, err := svc.List(ctx)
		if err != nil {
			return err
		}
		if len(groups) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No HA groups")
			return nil
		}

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "GROUP\tNAME\tSTATE\tVERDICT\tPRIMARY\tSTANDBY\tEVALUATED")
		for _, g := range groups {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				g.GroupID, g.Name, g.State, g.Verdict, g.Primary.SystemID, g.Standby.SystemID,
				ago(g.EvaluatedAt, g.Now))
		}
		return w.Flush()
	},
}

var haStatusCmd = &cobra.Command{
	Use:   "status <group>",
	Short: "Show an HA group's state, verdict and each node's latest report",
	Long: `Show an HA group by ID or name.

STATE governs what may be done next and changes only through a flow, an
operator, or a condition that has lasted. VERDICT is judged anew at every
evaluation, with its reasons. They are different on purpose: a broken
replication link is a verdict of error on a group whose state is degraded.

"Last sync confirmed" is the last evaluation at which both nodes reported the
link connected, both disks up to date and nothing out of sync. Data written
after it is not known to be on the standby.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		svc, err := newHAService(pool)
		if err != nil {
			return err
		}
		flows, err := newFlowService(pool, nil)
		if err != nil {
			return err
		}
		gs, err := svc.Get(ctx, args[0])
		if err != nil {
			return err
		}
		flow, err := flows.LatestFlow(ctx, gs.GroupID)
		if err != nil {
			return err
		}
		printHAGroup(cmd.OutOrStdout(), gs, flow, svc.Timings())
		return nil
	},
}

var haEventsCmd = &cobra.Command{
	Use:   "events <group>",
	Short: "List an HA group's state and verdict changes, in sequence order",
	Long: `List an HA group's events in sequence order.

The sequence belongs to the group and has no gaps: to follow a group, pass the
last sequence number seen as --after.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		pool, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()

		svc, err := newHAService(pool)
		if err != nil {
			return err
		}
		gs, events, err := svc.Events(ctx, args[0], haEventsAfter, haEventsLimit)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if len(events) == 0 {
			fmt.Fprintf(out, "No events for %s after %d\n", gs.Name, haEventsAfter)
			return nil
		}
		for _, e := range events {
			change := e.To
			if e.From != "" {
				change = e.From + " -> " + e.To
			}
			fmt.Fprintf(out, "%6d  %s  %-16s %s\n", e.Seq, e.At.Format(time.RFC3339), e.Kind, change)
			for _, r := range e.Reasons {
				fmt.Fprintf(out, "        %-9s %-7s %s\n", r.Severity, orDash(string(r.Node)), r.Message)
			}
		}
		return nil
	},
}

// printHAGroup writes a group for an operator, with its latest HA
// operation when it has had one.
func printHAGroup(out io.Writer, gs ha.GroupStatus, flow *ha.Flow, t ha.Timings) {
	fmt.Fprintf(out, "Group:      %s (%s)\n", gs.Name, gs.GroupID)
	fmt.Fprintf(out, "State:      %s\n", gs.State)
	fmt.Fprintf(out, "Verdict:    %s (evaluated %s)\n", gs.Verdict, ago(gs.EvaluatedAt, gs.Now))
	for _, r := range gs.Reasons {
		fmt.Fprintf(out, "            %-9s %-7s %s\n", r.Severity, orDash(string(r.Node)), r.Message)
	}

	switch {
	case gs.LastSyncConfirmedAt == nil:
		fmt.Fprintln(out, "Last sync confirmed: never")
	case gs.EvaluatedAt != nil && gs.EvaluatedAt.Sub(*gs.LastSyncConfirmedAt) > 0:
		fmt.Fprintf(out, "Last sync confirmed: %s — not confirmed for the %s before the last evaluation\n",
			gs.LastSyncConfirmedAt.Format(time.RFC3339),
			gs.EvaluatedAt.Sub(*gs.LastSyncConfirmedAt).Round(time.Second))
	default:
		fmt.Fprintf(out, "Last sync confirmed: %s (at the last evaluation)\n", gs.LastSyncConfirmedAt.Format(time.RFC3339))
	}

	r, s := gs.Resource, gs.Service
	fmt.Fprintf(out, "Resource:   %s on %s, mounted at %s, replication port %d\n", r.Name, r.Device, r.MountPoint, r.Port)
	service := fmt.Sprintf("%s, port %d", s.Unit, s.Port)
	if s.HealthPath != "" {
		service += ", health " + s.HealthPath
		if s.MaxHealthLatencyMs > 0 {
			service += fmt.Sprintf(" (limit %dms)", s.MaxHealthLatencyMs)
		}
	}
	if s.ExpectedGPUs > 0 {
		service += fmt.Sprintf(", %d GPU(s) expected", s.ExpectedGPUs)
	}
	fmt.Fprintf(out, "Service:    %s\n", service)
	if gs.ServiceAddress != "" {
		fmt.Fprintf(out, "Address:    %s\n", gs.ServiceAddress)
	}
	if flow != nil {
		printHAFlow(out, *flow)
	}

	for _, n := range []ha.Node{gs.Primary, gs.Standby} {
		fmt.Fprintln(out)
		printHANode(out, n, gs.Now, t)
	}
}

// printHAFlow writes an HA operation and the jobs of its steps.
func printHAFlow(out io.Writer, f ha.Flow) {
	line := fmt.Sprintf("%s %s, %s at step %d", f.Kind, f.FlowID, f.Status, f.Step)
	if f.FinishedAt != nil {
		line += ", ended " + f.FinishedAt.Format(time.RFC3339)
	} else {
		line += ", started " + f.StartedAt.Format(time.RFC3339)
	}
	fmt.Fprintf(out, "Operation:  %s\n", line)
	if f.Message != "" {
		fmt.Fprintf(out, "            %s\n", f.Message)
	}
	for _, j := range f.Jobs {
		fmt.Fprintf(out, "            step %d  %s  job %s  %s", j.Step, j.SystemID, j.JobID, j.Status)
		if j.Message != "" {
			fmt.Fprintf(out, ": %s", j.Message)
		}
		fmt.Fprintln(out)
	}
}

// printHANode writes one node's latest report.
func printHANode(out io.Writer, n ha.Node, now time.Time, t ha.Timings) {
	title := fmt.Sprintf("%s  %s  %s", n.Role, n.SystemID, n.Address)
	if n.Fenced {
		title += "  FENCED"
	}
	fmt.Fprintln(out, strings.ToUpper(title[:1])+title[1:])
	if n.Disk != "" {
		fmt.Fprintf(out, "  Data disk:    %s (node %d)\n", n.Disk, n.NodeID)
	}
	if n.Report == nil {
		fmt.Fprintln(out, "  Reported:     never under this role")
		return
	}
	r := n.Report
	reported := ago(n.ReceivedAt, now)
	if n.ReceivedAt != nil && now.Sub(*n.ReceivedAt) > t.StaleAfter {
		reported += " — silent"
	}
	fmt.Fprintf(out, "  Reported:     %s (agent %s, host %s)\n", reported, orDash(n.AgentID), orDash(r.Hostname))
	fmt.Fprintf(out, "  Service:      %s\n", describeUnitCheck(r.Service))
	fmt.Fprintf(out, "  Port:         %s\n", describePortCheck(r.Port))
	fmt.Fprintf(out, "  Health API:   %s\n", describeHTTPCheck(r.HealthAPI))
	if r.PeerPort != nil || r.PeerHealthAPI != nil {
		fmt.Fprintf(out, "  Probe port:   %s\n", describePortCheck(r.PeerPort))
		fmt.Fprintf(out, "  Probe API:    %s\n", describeHTTPCheck(r.PeerHealthAPI))
	}
	if r.GPU != nil {
		if r.GPU.Error != "" {
			fmt.Fprintf(out, "  GPUs:         unreadable: %s\n", r.GPU.Error)
		} else {
			fmt.Fprintf(out, "  GPUs:         %d\n", r.GPU.Count)
		}
	}
	fmt.Fprintf(out, "  Replication:  %s\n", describeReplication(r.Replication))
}

func describeUnitCheck(u *model.HAUnitCheck) string {
	switch {
	case u == nil:
		return "not measured"
	case u.Error != "":
		return "unreadable: " + u.Error
	case u.LoadState == "not-found":
		return "unit not found"
	default:
		return orDash(u.ActiveState) + "/" + orDash(u.SubState)
	}
}

func describePortCheck(p *model.HAPortCheck) string {
	switch {
	case p == nil:
		return "not measured"
	case p.Open:
		return "open (" + p.Address + ")"
	case p.Error != "":
		return "closed (" + p.Address + "): " + p.Error
	default:
		return "closed (" + p.Address + ")"
	}
}

func describeHTTPCheck(c *model.HAHTTPCheck) string {
	switch {
	case c == nil:
		return "not measured"
	case c.StatusCode == 0:
		return fmt.Sprintf("no answer after %dms: %s", c.LatencyMs, orDash(c.Error))
	default:
		return fmt.Sprintf("%d in %dms (%s)", c.StatusCode, c.LatencyMs, c.URL)
	}
}

func describeReplication(r *model.HAReplicationStatus) string {
	switch {
	case r == nil:
		return "not reported"
	case r.Error != "":
		return "unreadable: " + r.Error
	}
	link := orDash(r.Connection)
	if r.Sync != "" {
		link += "/" + r.Sync
	}
	s := fmt.Sprintf("%s/%s, %s, disk %s/%s", orDash(r.Role), orDash(r.PeerRole), link, orDash(r.Disk), orDash(r.PeerDisk))
	if r.OutOfSyncBytes != nil {
		s += ", " + describeBytes(r.OutOfSyncBytes) + " out of sync"
	} else {
		s += ", out-of-sync amount unknown"
	}
	return s
}

// ago renders a moment as its age against now, or "never".
func ago(t *time.Time, now time.Time) string {
	if t == nil {
		return "never"
	}
	return fmt.Sprintf("%s (%s ago)", t.Format(time.RFC3339), now.Sub(*t).Round(time.Second))
}

func init() {
	f := haCreateCmd.Flags()
	f.StringVar(&haCreateSpec.Name, "name", "", "group name: lowercase letters, digits and hyphens (required)")
	f.StringVar(&haCreateSpec.Primary.SystemID, "primary", "", "system ID of the primary (required)")
	f.StringVar(&haCreateSpec.Primary.Address, "primary-address", "", "private IP address of the primary (required)")
	f.StringVar(&haCreateSpec.Standby.SystemID, "standby", "", "system ID of the standby (required)")
	f.StringVar(&haCreateSpec.Standby.Address, "standby-address", "", "private IP address of the standby (required)")
	f.StringVar(&haCreateSpec.Primary.Disk, "primary-disk", "", "the primary's data disk, a /dev/disk/by-id path from its inventory")
	f.StringVar(&haCreateSpec.Standby.Disk, "standby-disk", "", "the standby's data disk, a /dev/disk/by-id path from its inventory")
	f.StringVar(&haCreateSpec.Resource.Name, "resource", "", "replication resource configured by hand to adopt, instead of the disks")
	f.StringVar(&haCreateSpec.Resource.Device, "device", "/dev/drbd0", "replicated device the filesystem sits on")
	f.StringVar(&haCreateSpec.Resource.MountPoint, "mount-point", "/data", "where the primary mounts the replicated volume")
	f.IntVar(&haCreateSpec.Resource.Port, "replication-port", 7789, "replication TCP port on both nodes")
	f.StringVar(&haCreateSpec.Service.Unit, "service-unit", "", "systemd unit of the protected service (required)")
	f.IntVar(&haCreateSpec.Service.Port, "service-port", 0, "TCP port the service listens on (required)")
	f.StringVar(&haCreateSpec.Service.HealthPath, "health-path", "", "HTTP path of the service's health API; empty skips the check")
	f.DurationVar(&haCreateMaxLatency, "max-health-latency", 0,
		fmt.Sprintf("health API latency above which the group needs attention, below %s; 0 skips the check",
			model.HAHealthAPITimeout))
	f.IntVar(&haCreateSpec.Service.ExpectedGPUs, "expected-gpus", 0, "GPUs each node must see; 0 skips the check")
	f.StringVar(&haCreateSpec.ServiceAddress, "service-address", "", "where clients reach the service, recorded for switches")
	for _, name := range []string{"name", "primary", "primary-address", "standby", "standby-address", "service-unit", "service-port"} {
		_ = haCreateCmd.MarkFlagRequired(name)
	}

	haEventsCmd.Flags().Int64Var(&haEventsAfter, "after", 0, "list events after this sequence number")
	haEventsCmd.Flags().IntVar(&haEventsLimit, "limit", 50, "how many events to list, at most 1000")

	haCmd.AddCommand(haCreateCmd, haListCmd, haStatusCmd, haEventsCmd, haSetupCmd, haTeardownCmd, haAcknowledgeCmd)
	rootCmd.AddCommand(haCmd)
}
