package rescue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/filelock"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

const (
	testRecoveryID = "1a2b3c4d5e6f7081"
	testToken      = "test-claim-token-0123456789abcdef"
	testSecret     = "test-secret-key-value"
	// testReportToken is what the fake framework issues with a claim.
	testReportToken = "test-report-token-0123456789"
)

// fakeFramework records the claims it receives and answers through respond,
// which gets the 1-based request number.
type fakeFramework struct {
	t       *testing.T
	mu      sync.Mutex
	keys    []string
	respond func(n int, w http.ResponseWriter)
}

func (f *fakeFramework) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	want := "/aiha/agent/restore/" + testRecoveryID + "/claim"
	if r.Method != http.MethodPost || r.URL.Path != want {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}
	var req model.RestoreClaimReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("decode claim: %v", err)
	}
	if req.ClaimToken != testToken {
		f.t.Errorf("claim token not sent")
	}
	f.mu.Lock()
	f.keys = append(f.keys, req.RequestKey)
	n := len(f.keys)
	f.mu.Unlock()
	f.respond(n, w)
}

func (f *fakeFramework) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

func okClaim(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(claimResponse())
}

func claimResponse() model.RestoreClaimResp {
	return model.RestoreClaimResp{
		RestoreID:   "9b1c2d3e4f5a6b7c",
		RecoveryID:  testRecoveryID,
		Plan:        json.RawMessage(`{"sourceSystemId":"web01-3f2a9c1e","sourceBackupSetId":"9c0d1e2f3a4b5c6d"}`),
		ReportToken: testReportToken,
		Repository: model.RepositoryAccess{
			Type: "s3", Provider: "Minio", Endpoint: "https://storage.example.com:9000",
			Bucket: "aiha-backup", Prefix: "systems/web01", AccessKey: "test-access", SecretKey: testSecret,
		},
	}
}

func refuse(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(model.SimpleMsg{Message: msg})
}

// startFramework serves f over TLS and returns its URL and certificate pin.
func startFramework(t *testing.T, f *fakeFramework) (string, []byte) {
	t.Helper()
	f.t = t
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	pin := sha256.Sum256(srv.Certificate().Raw)
	return srv.URL, pin[:]
}

func writeRecovery(t *testing.T, url string, pin []byte) string {
	t.Helper()
	return writeRecoveryFor(t, testRecoveryID, testToken, url, pin)
}

func writeRecoveryFor(t *testing.T, recoveryID, token, url string, pin []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recovery.env")
	content := fmt.Sprintf("AIHA_RECOVERY_ID=%s\nAIHA_FRAMEWORK_URL=%s\nAIHA_CLAIM_TOKEN=%s\nAIHA_FRAMEWORK_CERT_SHA256=%s\n",
		recoveryID, url, token, hex.EncodeToString(pin))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write recovery file: %v", err)
	}
	return path
}

// testOptions treats the output directory as RAM-backed through the test seam,
// so the tests do not depend on what filesystem the temporary directory is on.
func testOptions(t *testing.T, recoveryFile string) Options {
	t.Helper()
	return Options{
		RecoveryFile: recoveryFile,
		OutDir:       t.TempDir(),
		Timeout:      10 * time.Second,
		ramBacked:    func(string) (bool, error) { return true, nil },
		backoffBase:  time.Millisecond,
		backoffMax:   5 * time.Millisecond,
	}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = prev })
	return &buf
}

func TestRunStagesClaim(t *testing.T) {
	logs := captureLogs(t)
	f := &fakeFramework{respond: func(_ int, w http.ResponseWriter) { okClaim(w) }}
	url, pin := startFramework(t, f)
	opts := testOptions(t, writeRecovery(t, url, pin))

	res, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	dir := filepath.Join(opts.OutDir, testRecoveryID)
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("recovery directory missing or not 0700: %v", err)
	}
	for _, name := range []string{requestKeyFile, planFile, rcloneFile, reportTokenFile} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", name, info.Mode().Perm())
		}
	}

	rclone, _ := os.ReadFile(res.RcloneFile)
	for _, want := range []string{"[aiha]", "provider = Minio", "secret_access_key = " + testSecret, "endpoint = https://storage.example.com:9000"} {
		if !strings.Contains(string(rclone), want) {
			t.Fatalf("rclone.conf lacks %q:\n%s", want, rclone)
		}
	}
	plan, _ := os.ReadFile(res.PlanFile)
	if strings.Contains(string(plan), testSecret) || strings.Contains(string(plan), "test-access") {
		t.Fatalf("restore-plan.json holds credentials:\n%s", plan)
	}
	if strings.Contains(string(plan), testReportToken) {
		t.Fatalf("restore-plan.json holds the report token:\n%s", plan)
	}
	if strings.Contains(logs.String(), testToken) || strings.Contains(logs.String(), testSecret) ||
		strings.Contains(logs.String(), testReportToken) {
		t.Fatalf("logs contain a secret:\n%s", logs.String())
	}

	staged, err := LoadStaged(res.Dir)
	if err != nil {
		t.Fatalf("load staged: %v", err)
	}
	if staged.Plan.SourceSystemID != "web01-3f2a9c1e" || staged.Plan.SourceBackupSetID != "9c0d1e2f3a4b5c6d" {
		t.Fatalf("staged plan = %+v", staged.Plan)
	}
	if staged.ReportToken != testReportToken || staged.RcloneFile != res.RcloneFile ||
		staged.Repository.Bucket != "aiha-backup" || staged.RcloneRemote != RcloneRemote {
		t.Fatalf("staged claim = %+v", staged)
	}
}

func TestRunRefusesUnusableClaimResponse(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*model.RestoreClaimResp)
	}{
		{"plan with a file list", func(r *model.RestoreClaimResp) {
			r.Plan = json.RawMessage(`{"sourceSystemId":"web01","sourceBackupSetId":"s","items":[]}`)
		}},
		{"plan without a set", func(r *model.RestoreClaimResp) { r.Plan = json.RawMessage(`{"sourceSystemId":"web01"}`) }},
		{"no report token", func(r *model.RestoreClaimResp) { r.ReportToken = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeFramework{respond: func(_ int, w http.ResponseWriter) {
				resp := claimResponse()
				tt.mutate(&resp)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(resp)
			}}
			url, pin := startFramework(t, f)
			opts := testOptions(t, writeRecovery(t, url, pin))
			if _, err := Run(context.Background(), opts); err == nil {
				t.Fatal("run succeeded on an unusable claim response")
			}
			if _, err := os.Stat(filepath.Join(opts.OutDir, testRecoveryID, rcloneFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("credentials staged for an unusable response: %v", err)
			}
		})
	}
}

func TestRunRejectsWrongPin(t *testing.T) {
	f := &fakeFramework{respond: func(_ int, w http.ResponseWriter) { okClaim(w) }}
	url, _ := startFramework(t, f)
	other := sha256.Sum256([]byte("some other certificate"))

	_, err := Run(context.Background(), testOptions(t, writeRecovery(t, url, other[:])))
	if !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("err = %v, want ErrPinMismatch", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("server received %d requests over an unpinned connection", n)
	}
}

func TestRunRefusesRedirect(t *testing.T) {
	var (
		mu   sync.Mutex
		hits int
	)
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	t.Cleanup(target.Close)

	f := &fakeFramework{respond: func(_ int, w http.ResponseWriter) {
		w.Header().Set("Location", target.URL+"/steal")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}}
	url, pin := startFramework(t, f)

	_, err := Run(context.Background(), testOptions(t, writeRecovery(t, url, pin)))
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("err = %v, want ErrRedirect", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("the redirect target received %d requests", hits)
	}
}

func TestRunRetriesUnavailableWithSameKey(t *testing.T) {
	f := &fakeFramework{respond: func(n int, w http.ResponseWriter) {
		if n < 3 {
			refuse(w, http.StatusServiceUnavailable, "Service initializing, retry shortly")
			return
		}
		okClaim(w)
	}}
	url, pin := startFramework(t, f)

	if _, err := Run(context.Background(), testOptions(t, writeRecovery(t, url, pin))); err != nil {
		t.Fatalf("run: %v", err)
	}
	keys := f.requests()
	if len(keys) != 3 || keys[0] != keys[1] || keys[1] != keys[2] {
		t.Fatalf("want 3 requests under one key, got %v", keys)
	}
}

// TestRunRetriesAfterLostResponse drops the connection after the framework has
// recorded the claim, as when a response is lost in transit.
func TestRunRetriesAfterLostResponse(t *testing.T) {
	f := &fakeFramework{respond: func(n int, w http.ResponseWriter) {
		if n == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		okClaim(w)
	}}
	url, pin := startFramework(t, f)

	if _, err := Run(context.Background(), testOptions(t, writeRecovery(t, url, pin))); err != nil {
		t.Fatalf("run: %v", err)
	}
	keys := f.requests()
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("want the retry under the same key, got %v", keys)
	}
}

func TestRunStopsOnRefusal(t *testing.T) {
	f := &fakeFramework{respond: func(_ int, w http.ResponseWriter) {
		refuse(w, http.StatusConflict, "Recovery already claimed")
	}}
	url, pin := startFramework(t, f)

	_, err := Run(context.Background(), testOptions(t, writeRecovery(t, url, pin)))
	if !errors.Is(err, ErrClaimRefused) || !strings.Contains(err.Error(), "Recovery already claimed") {
		t.Fatalf("err = %v, want ErrClaimRefused with the framework's message", err)
	}
	if n := len(f.requests()); n != 1 {
		t.Fatalf("a refusal was retried: %d requests", n)
	}
}

func TestRunReusesRequestKeyAcrossRuns(t *testing.T) {
	f := &fakeFramework{respond: func(_ int, w http.ResponseWriter) { okClaim(w) }}
	url, pin := startFramework(t, f)
	opts := testOptions(t, writeRecovery(t, url, pin))

	for i := range 2 {
		if _, err := Run(context.Background(), opts); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	if keys := f.requests(); len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("a rerun used a new request key: %v", keys)
	}
}

func TestRunRefusesNonRAMOutDirBeforeClaiming(t *testing.T) {
	f := &fakeFramework{respond: func(_ int, w http.ResponseWriter) { okClaim(w) }}
	url, pin := startFramework(t, f)
	opts := testOptions(t, writeRecovery(t, url, pin))
	opts.ramBacked = func(string) (bool, error) { return false, nil }

	if _, err := Run(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "not on tmpfs") {
		t.Fatalf("err = %v, want a tmpfs refusal", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("claim sent despite the refusal: %d requests", n)
	}
	if _, err := os.Stat(filepath.Join(opts.OutDir, testRecoveryID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery directory created on a non-RAM filesystem: %v", err)
	}
}

func TestRunRefusesSecondAgent(t *testing.T) {
	f := &fakeFramework{respond: func(_ int, w http.ResponseWriter) { okClaim(w) }}
	url, pin := startFramework(t, f)
	opts := testOptions(t, writeRecovery(t, url, pin))

	dir := filepath.Join(opts.OutDir, testRecoveryID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := filelock.Acquire(context.Background(), filepath.Join(dir, "agent.lock"), 0)
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer held.Release()

	if _, err := Run(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "another agent") {
		t.Fatalf("err = %v, want a refusal while another agent runs", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("claim sent while another agent held the lock: %d requests", n)
	}
}

func TestLoadRecoveryRejectsBadValues(t *testing.T) {
	good := map[string]string{
		"AIHA_RECOVERY_ID":           testRecoveryID,
		"AIHA_FRAMEWORK_URL":         "https://framework.example.com:8443",
		"AIHA_CLAIM_TOKEN":           testToken,
		"AIHA_FRAMEWORK_CERT_SHA256": strings.Repeat("ab", 32),
		"AIHA_FRAMEWORK_CERT_PIN":    "true",
	}
	tests := []struct{ key, value string }{
		{"AIHA_RECOVERY_ID", "../etc"},
		{"AIHA_RECOVERY_ID", ""},
		{"AIHA_FRAMEWORK_URL", "http://framework.example.com"},
		{"AIHA_CLAIM_TOKEN", "short"},
		{"AIHA_FRAMEWORK_CERT_SHA256", "abcd"},
		{"AIHA_FRAMEWORK_CERT_PIN", "ture"},
		{"AIHA_FRAMEWORK_CERT_PIN", "false"}, // pinning off, yet a pin is still given
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			var b strings.Builder
			for k, v := range good {
				if k == tt.key {
					v = tt.value
				}
				fmt.Fprintf(&b, "%s=%s\n", k, v)
			}
			path := filepath.Join(t.TempDir(), "recovery.env")
			if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadRecovery(path)
			if err == nil {
				t.Fatal("accepted an invalid value")
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatalf("error exposes the claim token: %v", err)
			}
		})
	}
}

func TestLoadRecoveryWithPinningOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.env")
	env := "AIHA_RECOVERY_ID=" + testRecoveryID + "\nAIHA_FRAMEWORK_URL=https://framework.example.com\n" +
		"AIHA_CLAIM_TOKEN=" + testToken + "\nAIHA_FRAMEWORK_CERT_PIN=false\nAIHA_FRAMEWORK_CERT_SHA256=\n"
	if err := os.WriteFile(path, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err := LoadRecovery(path)
	if err != nil {
		t.Fatalf("pinning off was rejected: %v", err)
	}
	if rec.CertSHA256 != nil {
		t.Fatalf("pin = %x, want none", rec.CertSHA256)
	}
}

func TestRcloneConfigRejectsLineBreaks(t *testing.T) {
	r := model.RepositoryAccess{
		Type: "s3", Provider: "Minio", Endpoint: "https://storage.example.com",
		Bucket: "b", AccessKey: "a", SecretKey: "s\n[evil]\ntype = local",
	}
	if _, err := rcloneConfig(r); err == nil {
		t.Fatal("accepted a value with a line break")
	}
}

func TestReportClient(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
		results  int
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testReportToken {
			t.Errorf("Authorization = %q", got)
		}
		mu.Lock()
		requests = append(requests, r.URL.Path)
		n := 0
		if strings.HasSuffix(r.URL.Path, "/result") {
			results++
			n = results
		}
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/progress"):
			var req model.RestoreProgressReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Seq != 3 || req.Phase != "extracting" {
				t.Errorf("progress body = %+v, %v", req, err)
			}
			w.WriteHeader(http.StatusNoContent)
		case n == 1:
			refuse(w, http.StatusServiceUnavailable, "Restore report failed")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	pin := sha256.Sum256(srv.Certificate().Raw)
	rec := Recovery{RecoveryID: testRecoveryID, FrameworkURL: srv.URL, CertSHA256: pin[:]}

	c := NewReportClient(rec, testReportToken)
	c.c.backoffBase, c.c.backoffMax = time.Millisecond, 5*time.Millisecond
	ctx := context.Background()
	if err := c.Progress(ctx, model.RestoreProgressReq{Seq: 3, Phase: "extracting"}); err != nil {
		t.Fatalf("progress: %v", err)
	}
	if err := c.Result(ctx, model.RestoreResultReq{Outcome: "succeeded"}); err != nil {
		t.Fatalf("result: %v", err)
	}
	want := []string{
		"/aiha/agent/restore/" + testRecoveryID + "/progress",
		"/aiha/agent/restore/" + testRecoveryID + "/result",
		"/aiha/agent/restore/" + testRecoveryID + "/result",
	}
	if fmt.Sprint(requests) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v (the unavailable result retried once)", requests, want)
	}
}

func TestReportClientDoesNotRetryRefusal(t *testing.T) {
	var calls int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		refuse(w, http.StatusConflict, "A different result was already recorded for this restore")
	}))
	t.Cleanup(srv.Close)
	pin := sha256.Sum256(srv.Certificate().Raw)

	c := NewReportClient(Recovery{RecoveryID: testRecoveryID, FrameworkURL: srv.URL, CertSHA256: pin[:]}, testReportToken)
	err := c.Result(context.Background(), model.RestoreResultReq{Outcome: "failed"})
	if !errors.Is(err, ErrReportRefused) || calls != 1 {
		t.Fatalf("err = %v after %d call(s), want ErrReportRefused after 1", err, calls)
	}
}

// TestRunCreatesAMissingOutDir covers a run by hand, without autorun2 having
// made /run/aiha first.
func TestRunCreatesAMissingOutDir(t *testing.T) {
	f := &fakeFramework{respond: func(_ int, w http.ResponseWriter) { okClaim(w) }}
	url, pin := startFramework(t, f)
	opts := testOptions(t, writeRecovery(t, url, pin))
	opts.OutDir = filepath.Join(opts.OutDir, "aiha")

	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatalf("run: %v", err)
	}
	if info, err := os.Stat(opts.OutDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("out dir missing or not 0700: %v", err)
	}
}
