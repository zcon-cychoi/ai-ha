package rest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
)

// A claim carries only what its kind needs. An HA job's response must not
// carry an empty repository the agent could mistake for one to write to.
func TestClaimResponseCarriesOnlyWhatItsKindNeeds(t *testing.T) {
	ha, err := json.Marshal(model.AgentJobClaimResp{
		JobID: "job-1", Kind: model.JobKindReplicationSetup,
		Replication: &model.ReplicationAccess{SharedSecret: "s"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ha), `"repository"`) || !strings.Contains(string(ha), `"replication":{"sharedSecret":"s"}`) {
		t.Errorf("HA claim = %s", ha)
	}

	backup, err := json.Marshal(model.AgentJobClaimResp{
		JobID: "job-2", Kind: model.JobKindBackup,
		Repository: model.RepositoryAccess{Type: "s3", Bucket: "aiha-backup"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(backup), `"repository":{`) || strings.Contains(string(backup), `"replication"`) {
		t.Errorf("backup claim = %s", backup)
	}
}

// An HA job whose operation moved on is answered as a conflict, which the
// agent reads as "do not run this" at the claim and at the start report.
func TestFlowMovedIsAConflict(t *testing.T) {
	status, _ := jobStatusFor(fmt.Errorf("check: %w", job.ErrFlowMoved))
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
}
