// Package drbd reads the replication state of a DRBD resource (TM-AI-HA-02
// table 5-2): this node's role and disk, the link to its peer, the peer's role
// and disk, and how much is out of sync.
//
// It reads DRBD 9 through "drbdsetup status --json", the version and format
// fixed by the manual verification on the target kernel (plan/ha-h0.md). The
// text forms carry the same values but are written for people and change
// between releases. DRBD 8.4 has no JSON status and is not read: on such a node
// the command fails and the report says so, rather than being guessed at.
//
// Only state is read. The resource configuration holds the peers' shared
// secret, and "drbdsetup show" would print it. The tool runs as a separate
// process and is never linked: it is GPLv2.
package drbd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/cloud-barista/ai-ha/pkg/backup/sysinfo"
)

// resourcePattern is what a resource name must look like before it is handed
// to the tool. It cannot start with '-', where the tool would read an option.
var resourcePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// Status is a resource's replication state, in DRBD's words.
type Status struct {
	// Role and PeerRole are Primary, Secondary or, for a peer that cannot be
	// reached, Unknown.
	Role     string
	PeerRole string
	// Connection is the link to the peer: Connected, Connecting, StandAlone
	// and so on. Replication is what runs over it: Established once in sync,
	// SyncSource or SyncTarget while catching up, Off with no link. DRBD 9
	// reports the two apart; 8.4 folded the second into the first.
	Connection  string
	Replication string
	// Disk and PeerDisk are UpToDate, Inconsistent, Outdated, DUnknown for a
	// peer that cannot be reached, and so on.
	Disk     string
	PeerDisk string
	// OutOfSyncBytes is how much this node knows to differ from its peer. It
	// is nil when the tool did not report it, which is not the same as zero.
	OutOfSyncBytes *int64
}

// Reader reads resource state through a sysinfo.Runner.
type Reader struct {
	runner sysinfo.Runner
}

// NewReader returns a Reader that runs the tool through runner.
func NewReader(runner sysinfo.Runner) *Reader {
	return &Reader{runner: runner}
}

// Status reads resource's replication state.
func (r *Reader) Status(ctx context.Context, resource string) (Status, error) {
	if !resourcePattern.MatchString(resource) {
		return Status{}, fmt.Errorf("%q is not a replication resource name", resource)
	}
	out, err := r.runner.Run(ctx, "drbdsetup", "status", "--json", resource)
	if err != nil {
		return Status{}, fmt.Errorf("read the replication status of %s: %w", resource, err)
	}
	return parse(out, resource)
}

// statusJSON is the part of "drbdsetup status --json" read here.
type statusJSON struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Devices []struct {
		Volume    int    `json:"volume"`
		DiskState string `json:"disk-state"`
	} `json:"devices"`
	Connections []struct {
		PeerRole        string `json:"peer-role"`
		ConnectionState string `json:"connection-state"`
		PeerDevices     []struct {
			Volume           int    `json:"volume"`
			ReplicationState string `json:"replication-state"`
			PeerDiskState    string `json:"peer-disk-state"`
			// OutOfSyncKiB is in KiB, as DRBD counts it: an 8 GiB volume
			// reports a size of about 8.4 million.
			OutOfSyncKiB *int64 `json:"out-of-sync"`
		} `json:"peer_devices"`
	} `json:"connections"`
}

// parse reads the status of resource from the tool's output.
//
// A group replicates one volume to one peer (TM-AI-HA-02 table 3-3). Anything
// else is refused rather than read in part: a second volume or peer that went
// unreported would look like a resource in good order.
func parse(out []byte, resource string) (Status, error) {
	var resources []statusJSON
	if err := json.Unmarshal(out, &resources); err != nil {
		return Status{}, fmt.Errorf("read the replication status of %s: %w", resource, err)
	}
	if len(resources) != 1 || resources[0].Name != resource {
		return Status{}, fmt.Errorf("drbdsetup did not report resource %s alone", resource)
	}
	res := resources[0]

	switch {
	case len(res.Devices) != 1:
		return Status{}, fmt.Errorf("resource %s has %d volumes; one is supported", resource, len(res.Devices))
	case len(res.Connections) != 1:
		return Status{}, fmt.Errorf("resource %s has %d peers; one is supported", resource, len(res.Connections))
	}
	dev, conn := res.Devices[0], res.Connections[0]

	st := Status{
		Role:       res.Role,
		PeerRole:   conn.PeerRole,
		Connection: conn.ConnectionState,
		Disk:       dev.DiskState,
	}
	for _, pd := range conn.PeerDevices {
		if pd.Volume != dev.Volume {
			continue
		}
		st.Replication, st.PeerDisk = pd.ReplicationState, pd.PeerDiskState
		if pd.OutOfSyncKiB != nil {
			bytes := *pd.OutOfSyncKiB * 1024
			st.OutOfSyncBytes = &bytes
		}
	}

	// An empty state means the format is not the one this was written for. It
	// is reported as unreadable rather than judged as some state it is not.
	if st.Role == "" || st.Connection == "" || st.Disk == "" || st.Replication == "" {
		return Status{}, errors.New("drbdsetup reported an incomplete status: its output format may have changed")
	}
	return st, nil
}
