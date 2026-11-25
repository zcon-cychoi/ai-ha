//go:build !rescue

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/cloud-barista/ai-ha/pkg/agent/ha/replication"
	"github.com/cloud-barista/ai-ha/pkg/agent/worker"
	apimodel "github.com/cloud-barista/ai-ha/pkg/api/rest/model"
)

// fakeConfigurer records what the runners hand it.
type fakeConfigurer struct {
	secret string
	result replication.Result
	err    error
}

func (f *fakeConfigurer) Setup(_ context.Context, _ apimodel.ReplicationSetupSpec, secret string) (replication.Result, error) {
	f.secret = secret
	return f.result, f.err
}

func (f *fakeConfigurer) Teardown(context.Context, apimodel.ReplicationTeardownSpec) (replication.Result, error) {
	return f.result, f.err
}

func setupJob(t *testing.T, access *apimodel.ReplicationAccess) worker.Job {
	t.Helper()
	spec, err := json.Marshal(apimodel.ReplicationSetupSpec{Resource: "aiha-1", InitialPrimary: true})
	if err != nil {
		t.Fatal(err)
	}
	return worker.Job{ID: "j", Kind: apimodel.JobKindReplicationSetup, Spec: spec, Replication: access}
}

// The secret travels in the claim only. A claim without one is a fault, and
// the job stops before anything on the node is touched.
func TestReplicationSetupNeedsTheClaimsSecret(t *testing.T) {
	f := &fakeConfigurer{}
	_, err := replicationSetupRunner{c: f}.Run(context.Background(), setupJob(t, nil), nil)
	if err == nil || !strings.Contains(err.Error(), "no shared secret") {
		t.Fatalf("err = %v", err)
	}
	if f.secret != "" {
		t.Error("setup ran without a secret")
	}
}

// A failed setup still reports the steps it took, so that an operator sees
// where it stopped; the outcome message never carries the secret.
func TestReplicationSetupReportsItsStepsEitherWay(t *testing.T) {
	steps := replication.Result{Steps: []apimodel.ReplicationStep{
		{Name: replication.StepCreateMetadata, Done: true}, {Name: replication.StepFormat, Done: false},
	}, Role: "Primary", Disk: "UpToDate"}

	f := &fakeConfigurer{result: steps, err: errors.New("start ai.service: exit status 1")}
	out, err := replicationSetupRunner{c: f}.Run(context.Background(),
		setupJob(t, &apimodel.ReplicationAccess{SharedSecret: "s3cr3t-s3cr3t-s3cr3t"}), nil)
	if err == nil || out.Detail == nil {
		t.Fatalf("failed setup: outcome %+v, err %v", out, err)
	}
	if f.secret != "s3cr3t-s3cr3t-s3cr3t" {
		t.Errorf("secret = %q", f.secret)
	}

	f = &fakeConfigurer{result: steps}
	out, err = replicationSetupRunner{c: f}.Run(context.Background(),
		setupJob(t, &apimodel.ReplicationAccess{SharedSecret: "s3cr3t-s3cr3t-s3cr3t"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "Set up as primary of aiha-1 (Primary, disk UpToDate); did create-metadata; already so: format-volume"
	if out.Message != want {
		t.Errorf("message = %q, want %q", out.Message, want)
	}
	if strings.Contains(out.Message, "s3cr3t") {
		t.Error("the message carries the secret")
	}
}
