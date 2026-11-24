package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	backupmodel "github.com/cloud-barista/ai-ha/pkg/backup/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/agent"
	"github.com/cloud-barista/ai-ha/pkg/framework/inventory"
	"github.com/cloud-barista/ai-ha/pkg/framework/job"
	"github.com/cloud-barista/ai-ha/pkg/framework/repository"
	"github.com/cloud-barista/ai-ha/pkg/framework/restore"
	"github.com/cloud-barista/ai-ha/pkg/framework/secret"
	"github.com/cloud-barista/ai-ha/pkg/framework/token"
	"github.com/jackc/pgx/v5/pgxpool"
)

// e2eFramework is the whole API over a real PostgreSQL. It runs only when
// AIHA_TEST_DB_DSN points at a disposable database.
func e2eFramework(t *testing.T) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("AIHA_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("AIHA_TEST_DB_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	for name, migrate := range map[string]func(context.Context, *pgxpool.Pool) error{
		"repository": repository.Migrate, "restore": restore.Migrate, "agent": agent.Migrate,
		"job": job.Migrate, "inventory": inventory.Migrate,
	} {
		if err := migrate(ctx, pool); err != nil {
			t.Fatalf("migrate %s: %v", name, err)
		}
	}

	box, err := secret.NewBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	repos := repository.NewService(repository.NewPGStore(pool), box)
	jobs := job.NewService(job.NewPGStore(pool), repos, nil, job.TTLs{Backup: time.Hour, HA: 5 * time.Minute}, time.Minute,
		job.Timings{ProgressInterval: 10 * time.Second, LeaseTTL: 90 * time.Second})

	return NewHandler(Config{
		Agent:           agent.NewService(agent.NewPGStore(pool)),
		Jobs:            jobs,
		Inventory:       inventory.NewService(inventory.NewPGStore(pool)),
		Repositories:    repos,
		Restore:         restore.NewService(restore.NewPGStore(pool), jobs, repos, time.Hour, time.Minute),
		AdminAuth:       StaticAdminToken(token.Hash(testAdminToken)),
		FrameworkAccess: testFramework,
	}), pool
}

// send posts or gets body, expects status and decodes the response into out.
func send(t *testing.T, h http.Handler, method, path, authorization string, body any, status int, out any) {
	t.Helper()
	encoded := ""
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		encoded = string(raw)
	}
	rec := request(h, method, path, authorization, encoded)
	if rec.Code != status {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, rec.Code, status, rec.Body)
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: decode %s: %v", method, path, rec.Body, err)
		}
	}
}

// The demo scenario of roadmap.md, driven as a portal and an agent would: every
// step goes through the API, and only the rescue instance's own work is
// simulated. It stops at awaiting_boot, where the implementation stops.
func TestManagementAPIEndToEnd(t *testing.T) {
	h, pool := e2eFramework(t)
	const admin = "Bearer " + testAdminToken
	systemID := "sys-" + backupmodel.NewID()
	repoName := "e2e-" + backupmodel.NewID()

	var tokenID, repoID, jobID, restoreID string
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM jobs WHERE system_id = $1`, systemID)
		_, _ = pool.Exec(ctx, `DELETE FROM system_inventories WHERE system_id = $1`, systemID)
		_, _ = pool.Exec(ctx, `DELETE FROM agents WHERE system_id = $1`, systemID)
		_, _ = pool.Exec(ctx, `DELETE FROM registration_tokens WHERE token_id = $1`, tokenID)
		_, _ = pool.Exec(ctx, `DELETE FROM restore_jobs WHERE restore_id = $1`, restoreID)
		_, _ = pool.Exec(ctx, `DELETE FROM repositories WHERE repository_id = $1`, repoID)
	})

	// An operator issues a token and the agent enrols with it.
	var issued model.RegistrationTokenIssuedResp
	send(t, h, http.MethodPost, "/aiha/registration-tokens", admin,
		model.RegistrationTokenReq{Note: "e2e"}, http.StatusCreated, &issued)
	tokenID = issued.TokenID
	if issued.Framework != testFramework {
		t.Fatalf("framework access = %+v", issued.Framework)
	}

	var enrolled model.AgentRegisterResp
	send(t, h, http.MethodPost, "/aiha/agent/register", "", model.AgentRegisterReq{
		RegistrationToken: issued.Token, RegistrationKey: testRegistrationKey, SystemID: systemID, Hostname: "web01",
	}, http.StatusOK, &enrolled)
	agentAuthz := "Bearer " + enrolled.AgentToken

	// A system with no report yet cannot have its partitions chosen.
	var refused model.SimpleMsg
	send(t, h, http.MethodPost, "/aiha/systems/"+systemID+"/backups", admin,
		model.BackupReq{RepositoryID: repoName}, http.StatusConflict, &refused)
	if refused.Code != model.CodeNoInventory {
		t.Fatalf("code = %s", refused.Code)
	}

	send(t, h, http.MethodPost, "/aiha/agent/inventory", agentAuthz, model.AgentInventoryReq{
		CollectedAt: time.Now().UTC(), Complete: true, OSID: "ubuntu",
		Partitions: []model.InventoryPartition{
			{Device: "/dev/nvme0n1p1", MountPoint: "/", FSType: "ext4", FSUUID: "uuid-root", Candidate: true, Selectable: true},
			{Device: "/dev/nvme0n1p15", MountPoint: "/boot/efi", FSType: "vfat", FSUUID: "uuid-esp", Candidate: true, Selectable: true},
		},
	}, http.StatusOK, nil)

	var system model.ProtectedSystemDetail
	send(t, h, http.MethodGet, "/aiha/systems/"+systemID, admin, nil, http.StatusOK, &system)
	if system.AgentID != enrolled.AgentID || system.Hostname != "web01" ||
		system.Inventory == nil || !system.Inventory.Complete {
		t.Fatalf("system = %+v", system)
	}

	// The repository goes in through the API, and its credentials come back out
	// only to the agent that claims a job against it.
	var repo model.RepositoryInfo
	send(t, h, http.MethodPost, "/aiha/repositories", admin, model.RepositoryReq{
		Name: repoName, Provider: repository.ProviderMinio, Endpoint: "https://minio.example.com:9000", Bucket: "aiha-backup",
		RepositoryCredentialsReq: model.RepositoryCredentialsReq{AccessKey: "e2e-access", SecretKey: "e2e-secret"},
	}, http.StatusCreated, &repo)
	repoID = repo.ID

	// A backup is asked for, by repository name and with no devices.
	var accepted model.BackupAcceptedResp
	send(t, h, http.MethodPost, "/aiha/systems/"+systemID+"/backups", admin,
		model.BackupReq{RepositoryID: repoName}, http.StatusAccepted, &accepted)
	jobID = accepted.JobID
	if len(accepted.Devices) != 2 {
		t.Fatalf("devices = %v", accepted.Devices)
	}

	// The agent takes it through the southbound routes.
	var handed model.AgentJob
	send(t, h, http.MethodGet, "/aiha/agent/jobs?wait=5", agentAuthz, nil, http.StatusOK, &handed)
	if handed.JobID != jobID {
		t.Fatalf("handed %s, want %s", handed.JobID, jobID)
	}

	var claimed model.AgentJobClaimResp
	send(t, h, http.MethodPost, "/aiha/agent/jobs/"+jobID+"/claim", agentAuthz,
		model.AgentJobClaimReq{ClaimRequestKey: testRegistrationKey}, http.StatusOK, &claimed)
	if claimed.Repository.SecretKey != "e2e-secret" || claimed.Repository.Bucket != "aiha-backup" {
		t.Fatalf("repository handed to the agent = %+v", claimed.Repository)
	}
	// The agent picks its runner by kind; a backup carries no replication secret.
	if claimed.Kind != model.JobKindBackup || claimed.Replication != nil {
		t.Fatalf("claim kind = %q, replication = %+v", claimed.Kind, claimed.Replication)
	}
	var spec model.BackupJobSpec
	if err := json.Unmarshal(claimed.Spec, &spec); err != nil {
		t.Fatalf("spec: %v", err)
	}
	if spec.ExpectedFSUUIDs["/dev/nvme0n1p1"] != "uuid-root" || spec.RepositoryID != repoID {
		t.Fatalf("spec = %+v", spec)
	}

	send(t, h, http.MethodPost, "/aiha/agent/jobs/"+jobID+"/progress", agentAuthz, model.AgentJobProgressReq{
		AttemptID: claimed.AttemptID, LeaseToken: claimed.LeaseToken, Seq: 1,
		Phase: "archiving", ProcessedBytes: 1 << 20, Advanced: true,
	}, http.StatusOK, nil)

	// The portal follows it while it runs.
	var running model.JobInfo
	send(t, h, http.MethodGet, "/aiha/jobs/"+jobID, admin, nil, http.StatusOK, &running)
	if running.Status != string(job.StatusRunning) || running.Attempt == nil ||
		running.Attempt.Phase != "archiving" || running.Attempt.ProcessedBytes != 1<<20 {
		t.Fatalf("running job = %+v", running)
	}

	setID := backupmodel.NewID()
	detail, err := json.Marshal(model.BackupJobResult{
		BackupSetID: setID, Manifest: backupmodel.ManifestName(setID), Status: "completed", Items: 2,
	})
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	send(t, h, http.MethodPost, "/aiha/agent/jobs/"+jobID+"/result", agentAuthz, model.AgentJobResultReq{
		AttemptID: claimed.AttemptID, LeaseToken: claimed.LeaseToken,
		Outcome: "completed", Message: fmt.Sprintf("Backup set %s", setID), Detail: detail,
	}, http.StatusNoContent, nil)

	// The result shows in the history and by its set ID.
	var history model.BackupListResp
	send(t, h, http.MethodGet, "/aiha/backups?systemId="+systemID+"&status=completed", admin, nil, http.StatusOK, &history)
	if len(history.Items) != 1 || history.Items[0].BackupSetID != setID || history.Items[0].Items != 2 ||
		history.Items[0].ProcessedBytes != 1<<20 || history.Items[0].RequestedBy != adminSubject {
		t.Fatalf("history = %+v", history)
	}
	var backup model.BackupInfo
	send(t, h, http.MethodGet, "/aiha/backups/"+setID, admin, nil, http.StatusOK, &backup)
	if backup.JobID != jobID || backup.RepositoryID != repoID {
		t.Fatalf("backup = %+v", backup)
	}

	// A finished job cannot be cancelled.
	send(t, h, http.MethodPost, "/aiha/jobs/"+jobID+"/cancel", admin, nil, http.StatusConflict, nil)

	// The set is restored. The rescue agent uses only what the response gave.
	var restoreResp model.RestoreAcceptedResp
	send(t, h, http.MethodPost, "/aiha/restores", admin, model.RestoreReq{BackupSetID: setID}, http.StatusAccepted, &restoreResp)
	restoreID = restoreResp.RestoreID

	var rescue model.RestoreClaimResp
	send(t, h, http.MethodPost, "/aiha/agent/restore/"+restoreResp.Rescue.RecoveryID+"/claim", "", model.RestoreClaimReq{
		ClaimToken: restoreResp.Rescue.ClaimToken, RequestKey: testRegistrationKey,
	}, http.StatusOK, &rescue)
	if rescue.Repository.SecretKey != "e2e-secret" {
		t.Fatalf("repository handed to the rescue agent = %+v", rescue.Repository)
	}
	plan, err := model.DecodeRestorePlan(rescue.Plan)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.SourceSystemID != systemID || plan.SourceBackupSetID != setID {
		t.Fatalf("plan = %+v", plan)
	}
	reportAuthz := "Bearer " + rescue.ReportToken

	send(t, h, http.MethodPost, "/aiha/agent/restore/"+restoreResp.Rescue.RecoveryID+"/progress", reportAuthz, model.RestoreProgressReq{
		Seq: 1, Phase: "extracting", Step: 4, TotalSteps: 6, ProcessedBytes: 512, TotalBytes: 1024,
	}, http.StatusNoContent, nil)

	var restoring model.RestoreInfo
	send(t, h, http.MethodGet, "/aiha/restores/"+restoreID, admin, nil, http.StatusOK, &restoring)
	if restoring.Status != string(restore.StatusRestoring) || restoring.Progress == nil ||
		restoring.Progress.Step != 4 || restoring.Progress.TotalUnknown {
		t.Fatalf("restoring = %+v", restoring)
	}

	send(t, h, http.MethodPost, "/aiha/agent/restore/"+restoreResp.Rescue.RecoveryID+"/result", reportAuthz, model.RestoreResultReq{
		Outcome: "succeeded", Phase: "bootloader", Message: "Restored 2 partition(s)",
	}, http.StatusNoContent, nil)

	var restores model.RestoreListResp
	send(t, h, http.MethodGet, "/aiha/restores?systemId="+systemID, admin, nil, http.StatusOK, &restores)
	if len(restores.Items) != 1 {
		t.Fatalf("restores = %+v", restores)
	}
	finished := restores.Items[0]
	if finished.RestoreID != restoreID || finished.Status != string(restore.StatusAwaitingBoot) ||
		finished.RestoreOutcome != string(restore.OutcomeSucceeded) ||
		finished.BootVerification != bootVerificationPending || finished.SourceBackupSetID != setID {
		t.Fatalf("finished restore = %+v", finished)
	}

	// Revoking the agent takes effect on its next request and shows on the system.
	send(t, h, http.MethodPost, "/aiha/agents/"+enrolled.AgentID+"/revoke", admin, nil, http.StatusOK, nil)
	send(t, h, http.MethodGet, "/aiha/agent/jobs?wait=5", agentAuthz, nil, http.StatusUnauthorized, nil)
	send(t, h, http.MethodGet, "/aiha/systems/"+systemID, admin, nil, http.StatusOK, &system)
	if system.AgentStatus != agent.StateRevoked {
		t.Fatalf("agent status = %s", system.AgentStatus)
	}
	send(t, h, http.MethodPost, "/aiha/systems/"+systemID+"/backups", admin,
		model.BackupReq{RepositoryID: repoName}, http.StatusConflict, nil)

	// The token was spent, and listing it shows that without revealing it.
	rec := request(h, http.MethodGet, "/aiha/registration-tokens", admin, "")
	expectAbsent(t, rec, issued.Token)
	expectStatus(t, rec, http.StatusOK)
	var tokens model.RegistrationTokenListResp
	if err := json.Unmarshal(rec.Body.Bytes(), &tokens); err != nil {
		t.Fatalf("tokens: %v", err)
	}
	var spent bool
	for _, tok := range tokens.Items {
		spent = spent || (tok.TokenID == tokenID && tok.Status == agent.TokenStateUsedUp)
	}
	if !spent {
		t.Fatalf("the token does not show as used: %+v", tokens.Items)
	}

	// Nothing a caller reads may carry the repository's secret.
	for _, path := range []string{
		"/aiha/jobs/" + jobID, "/aiha/backups/" + setID, "/aiha/repositories/" + repoID,
		"/aiha/restores/" + restoreID, "/aiha/systems/" + systemID,
	} {
		assertNoRepositorySecret(t, request(h, http.MethodGet, path, admin, ""))
	}
}

func assertNoRepositorySecret(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	expectStatus(t, rec, http.StatusOK)
	expectAbsent(t, rec, "e2e-secret", "e2e-access")
}
