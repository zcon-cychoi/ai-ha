//go:build !rescue

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/agent/ha/replication"
	"github.com/cloud-barista/ai-ha/pkg/agent/worker"
	apimodel "github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/backup/sysinfo"
)

// replicationCommandTimeout bounds one command of a replication job. Making
// the metadata or the filesystem of a large volume takes longer than the
// inventory's commands, which are bounded at 30 seconds.
const replicationCommandTimeout = 5 * time.Minute

// replicationConfigurer is what the replication runners drive.
type replicationConfigurer interface {
	Setup(ctx context.Context, spec apimodel.ReplicationSetupSpec, secret string) (replication.Result, error)
	Teardown(ctx context.Context, spec apimodel.ReplicationTeardownSpec) (replication.Result, error)
}

// newReplicationConfigurer returns the Configurer the agent's jobs use.
func newReplicationConfigurer() *replication.Configurer {
	return replication.New(sysinfo.ExecRunner{Timeout: replicationCommandTimeout})
}

// replicationSetupRunner carries out replication-setup jobs.
type replicationSetupRunner struct{ c replicationConfigurer }

// Run implements worker.Runner. The steps taken are reported whether the
// job succeeds or not, so that an operator sees where a failed setup stopped.
func (r replicationSetupRunner) Run(ctx context.Context, j worker.Job, _ *atomic.Int64) (worker.Outcome, error) {
	var spec apimodel.ReplicationSetupSpec
	if err := json.Unmarshal(j.Spec, &spec); err != nil {
		return worker.Outcome{}, fmt.Errorf("decode the replication setup job: %w", err)
	}
	if j.Replication == nil {
		return worker.Outcome{}, errors.New("the claim carried no shared secret for the replication resource")
	}
	res, err := r.c.Setup(ctx, spec, j.Replication.SharedSecret)
	if err != nil {
		return worker.Outcome{Detail: res}, err
	}
	role := "standby"
	if spec.InitialPrimary {
		role = "primary"
	}
	return worker.Outcome{
		Message: fmt.Sprintf("Set up as %s of %s (%s, disk %s); %s", role, spec.Resource, res.Role, res.Disk, describeSteps(res)),
		Detail:  res,
	}, nil
}

// replicationTeardownRunner carries out replication-teardown jobs.
type replicationTeardownRunner struct{ c replicationConfigurer }

// Run implements worker.Runner.
func (r replicationTeardownRunner) Run(ctx context.Context, j worker.Job, _ *atomic.Int64) (worker.Outcome, error) {
	var spec apimodel.ReplicationTeardownSpec
	if err := json.Unmarshal(j.Spec, &spec); err != nil {
		return worker.Outcome{}, fmt.Errorf("decode the replication teardown job: %w", err)
	}
	res, err := r.c.Teardown(ctx, spec)
	if err != nil {
		return worker.Outcome{Detail: res}, err
	}
	return worker.Outcome{
		Message: fmt.Sprintf("Took %s down; the data and its metadata are still on the disk; %s", spec.Resource, describeSteps(res)),
		Detail:  res,
	}, nil
}

// describeSteps says which steps a job did and which it found already done.
func describeSteps(res replication.Result) string {
	var done, already []string
	for _, s := range res.Steps {
		if s.Done {
			done = append(done, s.Name)
		} else {
			already = append(already, s.Name)
		}
	}
	switch {
	case len(done) == 0:
		return "nothing needed doing"
	case len(already) == 0:
		return "did " + strings.Join(done, ", ")
	default:
		return "did " + strings.Join(done, ", ") + "; already so: " + strings.Join(already, ", ")
	}
}
