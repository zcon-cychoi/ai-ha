package ha

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
)

// Reason codes. A code names a condition and stays the same while it lasts,
// which is what decides whether a new evaluation is a change worth an event.
const (
	codePrimaryReportLost    = "primary_report_lost"
	codePrimaryServiceDown   = "primary_service_down"
	codePrimaryPortClosed    = "primary_port_closed"
	codePrimaryHealthFailed  = "primary_health_failed"
	codePrimaryHealthSlow    = "primary_health_slow"
	codeStandbyReportLate    = "standby_report_late"
	codeStandbyServiceOn     = "standby_service_running"
	codeStandbyServiceUnread = "standby_service_unknown"
	codeGPUMissing           = "gpu_missing"
	codeProbePortFailed      = "probe_port_failed"
	codeProbeHealthFailed    = "probe_health_failed"
	codeReplicationMissing   = "replication_unreported"
	codeReplicationUnread    = "replication_unreadable"
	codeBothPrimary          = "both_primary"
	codeRoleMismatch         = "role_mismatch"
	codeLinkDown             = "link_down"
	codeLinkDownLasting      = "link_down_persistent"
	codeResyncing            = "resyncing"
	codeOutOfSync            = "out_of_sync"
	codeSyncUnconfirmed      = "sync_unconfirmed"
	codeDiskBad              = "disk_bad"
	codeDiskUnclear          = "disk_unclear"
)

// DRBD's words. Both the 8.4 and the 9 vocabularies are listed: 8.4 reports a
// running resynchronization as the connection state, 9 as a separate
// replication state, and a node reports whichever its tool gives.
const (
	drbdPrimary   = "Primary"
	drbdConnected = "Connected"
	drbdUpToDate  = "UpToDate"
)

// linkUp are the connection states in which the peers are talking.
var linkUp = setOf("Connected",
	"StartingSyncS", "StartingSyncT", "WFBitMapS", "WFBitMapT", "WFSyncUUID",
	"SyncSource", "SyncTarget", "PausedSyncS", "PausedSyncT",
	"VerifyS", "VerifyT", "Ahead", "Behind")

// resync are the states in which one side is catching up with the other.
// Ahead and Behind are included: the peer is connected but behind.
var resync = setOf(
	"StartingSyncS", "StartingSyncT", "WFBitMapS", "WFBitMapT", "WFSyncUUID",
	"SyncSource", "SyncTarget", "PausedSyncS", "PausedSyncT", "Ahead", "Behind")

// badDisk are the disk states that mean the data cannot be relied on.
var badDisk = setOf("Inconsistent", "Outdated", "Failed", "Diskless", "Detaching")

// unitRunning are the systemd states of a service that holds the volume.
var unitRunning = setOf("active", "reloading", "activating", "deactivating")

func setOf(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// assessment is what one evaluation found.
type assessment struct {
	verdict Verdict
	reasons []Reason
	// syncConfirmed says both nodes reported the link connected, both disks
	// up to date and nothing out of sync.
	syncConfirmed bool
	bothPrimary   bool
	// impaired says the standby could not take over with current data right
	// now: the link, the resync, the standby itself or a disk.
	impaired bool
	// primaryFailing says the primary is silent or its service is down;
	// primaryFailingSince is since when, and failingLong whether that has
	// lasted.
	primaryFailing      bool
	primaryFailingSince *time.Time
	failingLong         bool
	linkDownSince       *time.Time
}

// judgement accumulates reasons.
type judgement struct {
	reasons  []Reason
	impaired bool
}

func (j *judgement) add(code string, sev Verdict, node Role, impairs bool, format string, args ...any) {
	j.reasons = append(j.reasons, Reason{Code: code, Severity: sev, Node: node, Message: fmt.Sprintf(format, args...)})
	if impairs {
		j.impaired = true
	}
}

func (j *judgement) verdict() Verdict {
	v := VerdictOK
	for _, r := range j.reasons {
		if r.Severity.severity() > v.severity() {
			v = r.Severity
		}
	}
	return v
}

// Assess evaluates a group at gs.Now and decides what changes.
//
// staleAfter is how old a node's last report may be before the node counts
// as silent, and how long a condition that only matters once it lasts — a
// broken link, a failing primary — must last before it does.
//
// It is a pure function of its arguments, so that the rules of TM-AI-HA-02
// tables 5-1 and 5-3 can be tested as a table.
func Assess(gs GroupStatus, staleAfter time.Duration) Change {
	a := assess(gs, staleAfter)
	next := nextState(gs.State, a, gs.Primary.Fenced || gs.Standby.Fenced)

	ch := Change{
		State:               next,
		Verdict:             a.verdict,
		Reasons:             a.reasons,
		LastSyncConfirmedAt: gs.LastSyncConfirmedAt,
		LinkDownSince:       a.linkDownSince,
		PrimaryFailingSince: a.primaryFailingSince,
	}
	if a.syncConfirmed {
		now := gs.Now
		ch.LastSyncConfirmedAt = &now
	}
	if a.verdict != gs.Verdict || !sameConditions(a.reasons, gs.Reasons) {
		ch.Events = append(ch.Events, NewEvent{
			Kind: EventVerdictChanged, From: string(gs.Verdict), To: string(a.verdict), Reasons: a.reasons,
		})
	}
	if next != gs.State {
		ch.Events = append(ch.Events, NewEvent{
			Kind: EventStateChanged, From: string(gs.State), To: string(next), Reasons: a.reasons,
		})
	}
	return ch
}

// assess applies the rules of TM-AI-HA-02 tables 5-1 and 5-3.
func assess(gs GroupStatus, staleAfter time.Duration) assessment {
	now := gs.Now
	svc := gs.Service
	p, s := gs.Primary, gs.Standby
	pFresh, sFresh := fresh(p, now, staleAfter), fresh(s, now, staleAfter)

	j := &judgement{}
	primaryFailing := false

	// The primary's service, as the primary sees it.
	if !pFresh {
		primaryFailing = true
		j.add(codePrimaryReportLost, VerdictError, RolePrimary, false, "%s; the cause is unknown", silence("primary", p, now))
	} else {
		r := p.Report
		if !unitActive(r.Service) {
			primaryFailing = true
			j.add(codePrimaryServiceDown, VerdictError, RolePrimary, false, "%s", describeUnit(svc.Unit, "primary", r.Service))
		}
		if r.Port == nil || !r.Port.Open {
			primaryFailing = true
			j.add(codePrimaryPortClosed, VerdictError, RolePrimary, false,
				"Port %d on the primary is not accepting connections", svc.Port)
		}
		if svc.HealthPath != "" {
			switch {
			case !httpOK(r.HealthAPI):
				primaryFailing = true
				j.add(codePrimaryHealthFailed, VerdictError, RolePrimary, false, "%s", describeHTTP("primary", r.HealthAPI))
			case svc.MaxHealthLatencyMs > 0 && r.HealthAPI.LatencyMs > int64(svc.MaxHealthLatencyMs):
				j.add(codePrimaryHealthSlow, VerdictAttention, RolePrimary, false,
					"The health API on the primary took %dms, above the %dms limit",
					r.HealthAPI.LatencyMs, svc.MaxHealthLatencyMs)
			}
		}
		if svc.ExpectedGPUs > 0 && gpuShort(r.GPU, svc.ExpectedGPUs) {
			j.add(codeGPUMissing, VerdictAttention, RolePrimary, false, "%s", describeGPU("primary", r.GPU, svc.ExpectedGPUs))
		}
	}

	// The standby: ready to take over, and not running the service.
	if !sFresh {
		j.add(codeStandbyReportLate, VerdictAttention, RoleStandby, true, "%s", silence("standby", s, now))
	} else {
		r := s.Report
		switch {
		case r.Service == nil || r.Service.Error != "" || r.Service.ActiveState == "":
			j.add(codeStandbyServiceUnread, VerdictAttention, RoleStandby, true,
				"The state of %s on the standby could not be read", svc.Unit)
		case unitRunning[r.Service.ActiveState]:
			j.add(codeStandbyServiceOn, VerdictAttention, RoleStandby, true,
				"%s is %s on the standby, where it must be stopped", svc.Unit, r.Service.ActiveState)
		}
		if svc.ExpectedGPUs > 0 && gpuShort(r.GPU, svc.ExpectedGPUs) {
			j.add(codeGPUMissing, VerdictAttention, RoleStandby, true, "%s", describeGPU("standby", r.GPU, svc.ExpectedGPUs))
		}
		// Only worth saying while the primary answers locally: otherwise the
		// primary's own failure is the reason, and this would repeat it.
		if pFresh && p.Report.Port != nil && p.Report.Port.Open {
			switch {
			case r.PeerPort == nil || !r.PeerPort.Open:
				j.add(codeProbePortFailed, VerdictAttention, RoleStandby, false,
					"The standby cannot reach port %d on the primary", svc.Port)
			case svc.HealthPath != "" && httpOK(p.Report.HealthAPI) && !httpOK(r.PeerHealthAPI):
				j.add(codeProbeHealthFailed, VerdictAttention, RoleStandby, false,
					"The standby cannot reach the health API on the primary")
			}
		}
	}

	// Replication, from whichever nodes reported in time.
	reps := map[Role]*model.HAReplicationStatus{}
	for _, n := range []struct {
		node  Node
		fresh bool
	}{{p, pFresh}, {s, sFresh}} {
		if !n.fresh {
			continue
		}
		rep := n.node.Report.Replication
		switch {
		case rep == nil:
			j.add(codeReplicationMissing, VerdictError, n.node.Role, true,
				"The %s did not report replication status", n.node.Role)
		case rep.Error != "":
			j.add(codeReplicationUnread, VerdictError, n.node.Role, true,
				"Replication status on the %s could not be read", n.node.Role)
		default:
			reps[n.node.Role] = rep
		}
	}
	pr, sr := reps[RolePrimary], reps[RoleStandby]
	observed := make([]*model.HAReplicationStatus, 0, 2)
	roles := make([]Role, 0, 2)
	for _, role := range []Role{RolePrimary, RoleStandby} {
		if rep := reps[role]; rep != nil {
			observed = append(observed, rep)
			roles = append(roles, role)
		}
	}

	bothPrimary := (pr != nil && sr != nil && pr.Role == drbdPrimary && sr.Role == drbdPrimary) ||
		(pr != nil && pr.Role == drbdPrimary && pr.PeerRole == drbdPrimary) ||
		(sr != nil && sr.Role == drbdPrimary && sr.PeerRole == drbdPrimary)
	if bothPrimary {
		j.add(codeBothPrimary, VerdictError, "", true, "Both nodes are Primary: their data may have diverged")
	} else {
		if pr != nil && pr.Role != drbdPrimary {
			j.add(codeRoleMismatch, VerdictError, RolePrimary, true,
				"The primary's replica is %s, not Primary", orUnknown(pr.Role))
		}
		if sr != nil && sr.Role == drbdPrimary {
			j.add(codeRoleMismatch, VerdictError, RoleStandby, true, "The standby's replica is Primary, not Secondary")
		}
	}

	linkDown, downState := false, ""
	for _, rep := range observed {
		if !linkUp[rep.Connection] {
			linkDown, downState = true, rep.Connection
		}
	}
	linkDownSince := gs.LinkDownSince
	switch {
	case linkDown:
		if linkDownSince == nil {
			t := now
			linkDownSince = &t
		}
		lasted := now.Sub(*linkDownSince).Round(time.Second)
		if lasted >= staleAfter {
			j.add(codeLinkDownLasting, VerdictError, "", true,
				"Replication has been disconnected (%s) for %s", orUnknown(downState), lasted)
		} else {
			j.add(codeLinkDown, VerdictAttention, "", true,
				"Replication is disconnected (%s) for %s", orUnknown(downState), lasted)
		}
	case len(observed) > 0:
		linkDownSince = nil
	}

	resyncing := false
	var outOfSync int64
	unmeasured := false
	for _, rep := range observed {
		if resync[rep.Connection] || resync[rep.Sync] {
			resyncing = true
		}
		if rep.OutOfSyncBytes == nil {
			unmeasured = true
		} else {
			outOfSync = max(outOfSync, *rep.OutOfSyncBytes)
		}
	}
	if !linkDown && len(observed) > 0 {
		switch {
		case resyncing:
			j.add(codeResyncing, VerdictAttention, "", true,
				"Resynchronization in progress (%s out of sync)", humanBytes(outOfSync))
		case outOfSync > 0:
			j.add(codeOutOfSync, VerdictAttention, "", true, "%s is out of sync", humanBytes(outOfSync))
		case unmeasured:
			j.add(codeSyncUnconfirmed, VerdictAttention, "", true, "The amount out of sync was not reported")
		}
	}

	for i, rep := range observed {
		role := roles[i]
		switch {
		case rep.Disk == drbdUpToDate:
		case resyncing && (rep.Disk == "Inconsistent" || rep.Disk == "Outdated"):
			// Part of the resynchronization reported above.
		case badDisk[rep.Disk]:
			j.add(codeDiskBad, VerdictError, role, true, "The %s's disk is %s", role, rep.Disk)
		default:
			j.add(codeDiskUnclear, VerdictAttention, role, true,
				"The %s's disk is %s, not UpToDate", role, orUnknown(rep.Disk))
		}
	}

	syncConfirmed := pr != nil && sr != nil && !bothPrimary && !resyncing &&
		pr.Connection == drbdConnected && sr.Connection == drbdConnected &&
		pr.Disk == drbdUpToDate && sr.Disk == drbdUpToDate &&
		pr.OutOfSyncBytes != nil && *pr.OutOfSyncBytes == 0 &&
		sr.OutOfSyncBytes != nil && *sr.OutOfSyncBytes == 0

	var failingSince *time.Time
	if primaryFailing {
		switch {
		case gs.PrimaryFailingSince != nil:
			failingSince = gs.PrimaryFailingSince
		case !pFresh && p.ReceivedAt != nil:
			// Silent since its last report, which is when the failure began
			// as far as anyone can tell.
			t := *p.ReceivedAt
			failingSince = &t
		default:
			t := now
			failingSince = &t
		}
	}

	return assessment{
		verdict:             j.verdict(),
		reasons:             j.reasons,
		syncConfirmed:       syncConfirmed,
		bothPrimary:         bothPrimary,
		impaired:            j.impaired,
		primaryFailing:      primaryFailing,
		primaryFailingSince: failingSince,
		failingLong:         failingSince != nil && now.Sub(*failingSince) >= staleAfter,
		linkDownSince:       linkDownSince,
	}
}

// nextState is the state an evaluation moves a group to (plan/ha-roadmap.md
// section 6). Only the states the evaluator owns move; a group being
// registered, configured, switched or held in error is left to whoever holds
// it.
func nextState(cur State, a assessment, fenced bool) State {
	switch cur {
	case StateSyncing, StateProtected, StateDegraded, StateFailoverPending:
	default:
		return cur
	}

	// Two writers is the one condition that cannot wait for anyone: an
	// operator has to look before anything else happens to the group.
	if a.bothPrimary {
		return StateError
	}
	healthy := a.verdict == VerdictOK && a.syncConfirmed && !fenced

	switch cur {
	case StateSyncing:
		// The initial synchronization ends only by finishing it.
		if healthy {
			return StateProtected
		}
		return cur
	case StateFailoverPending:
		// The primary recovering is the only way back; a switch is the
		// operator's way forward.
		if a.primaryFailing {
			return cur
		}
		if healthy {
			return StateProtected
		}
		return StateDegraded
	default: // protected, degraded
		switch {
		case a.failingLong:
			return StateFailoverPending
		case healthy:
			return StateProtected
		case a.impaired || fenced:
			return StateDegraded
		default:
			return cur
		}
	}
}

// fresh reports whether a node has reported under its role recently enough to
// be judged by what it said.
func fresh(n Node, now time.Time, staleAfter time.Duration) bool {
	return n.Report != nil && n.ReceivedAt != nil && now.Sub(*n.ReceivedAt) <= staleAfter
}

// sameConditions reports whether two reason lists name the same conditions,
// ignoring their messages, whose figures change at every evaluation.
func sameConditions(a, b []Reason) bool {
	return slices.Equal(conditionKeys(a), conditionKeys(b))
}

func conditionKeys(rs []Reason) []string {
	keys := make([]string, 0, len(rs))
	for _, r := range rs {
		keys = append(keys, string(r.Node)+"/"+r.Code)
	}
	slices.Sort(keys)
	return keys
}

func unitActive(u *model.HAUnitCheck) bool {
	return u != nil && u.Error == "" && (u.ActiveState == "active" || u.ActiveState == "reloading")
}

func httpOK(c *model.HAHTTPCheck) bool {
	return c != nil && c.Error == "" && c.StatusCode >= 200 && c.StatusCode < 300
}

func gpuShort(g *model.HAGPUCheck, expected int) bool {
	return g == nil || g.Error != "" || g.Count < expected
}

func silence(who string, n Node, now time.Time) string {
	if n.ReceivedAt == nil {
		return fmt.Sprintf("The %s has not reported yet", who)
	}
	return fmt.Sprintf("No report from the %s for %s", who, now.Sub(*n.ReceivedAt).Round(time.Second))
}

func describeUnit(unit, who string, u *model.HAUnitCheck) string {
	switch {
	case u == nil:
		return fmt.Sprintf("The %s did not report the state of %s", who, unit)
	case u.Error != "":
		return fmt.Sprintf("The state of %s on the %s could not be read", unit, who)
	case u.LoadState == "not-found":
		return fmt.Sprintf("%s does not exist on the %s", unit, who)
	default:
		return fmt.Sprintf("%s on the %s is %s", unit, who, orUnknown(u.ActiveState))
	}
}

func describeHTTP(who string, c *model.HAHTTPCheck) string {
	switch {
	case c == nil:
		return fmt.Sprintf("The %s did not report its health API", who)
	case c.StatusCode == 0:
		return fmt.Sprintf("The health API on the %s did not answer", who)
	default:
		return fmt.Sprintf("The health API on the %s answered %d", who, c.StatusCode)
	}
}

func describeGPU(who string, g *model.HAGPUCheck, expected int) string {
	if g == nil || g.Error != "" {
		return fmt.Sprintf("The %s could not count its GPUs (%d expected)", who, expected)
	}
	return fmt.Sprintf("The %s sees %d of %d expected GPUs", who, g.Count, expected)
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// humanBytes renders a byte count in binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
