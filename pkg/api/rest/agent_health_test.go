package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloud-barista/ai-ha/pkg/api/rest/model"
	"github.com/cloud-barista/ai-ha/pkg/framework/agent"
	"github.com/cloud-barista/ai-ha/pkg/framework/ha"
)

// haStore holds one group and records which system each report was filed
// under. The judging rules are covered in package ha; what is under test here
// is the HTTP layer and that the node is the token's, not the body's.
type haStore struct {
	group     ha.GroupStatus
	reporters []string
	reports   []model.AgentHealthReq
}

func (s *haStore) Create(context.Context, ha.NewGroup) (ha.GroupStatus, error) {
	return ha.GroupStatus{}, nil
}
func (s *haStore) List(context.Context) ([]ha.GroupStatus, error) { return nil, nil }
func (s *haStore) Get(context.Context, string) (ha.GroupStatus, error) {
	return s.group, nil
}
func (s *haStore) GroupIDs(context.Context) ([]string, error) { return nil, nil }
func (s *haStore) Events(context.Context, string, int64, int) ([]ha.Event, error) {
	return nil, nil
}
func (s *haStore) Evaluate(context.Context, string, ha.Decide) (ha.GroupStatus, error) {
	return s.group, nil
}

func (s *haStore) Report(_ context.Context, systemID, _ string, r model.AgentHealthReq, _ ha.Decide) (ha.GroupStatus, bool, error) {
	s.reporters = append(s.reporters, systemID)
	if systemID != s.group.Primary.SystemID && systemID != s.group.Standby.SystemID {
		return ha.GroupStatus{}, false, ha.ErrNotAssigned
	}
	s.reports = append(s.reports, r)
	return s.group, true, nil
}

func newHealthHandler(t *testing.T, primarySystem string) (http.Handler, *haStore) {
	t.Helper()
	store := &haStore{group: ha.GroupStatus{
		Group: ha.Group{
			GroupID: "6a7b8c9d0e1f2a3b", Name: "inference-ha", State: ha.StateRegistered,
			Resource: ha.ResourceSpec{Name: "r0", Device: "/dev/drbd0", MountPoint: "/data", Port: 7789},
			Service:  ha.ServiceSpec{Unit: "ai.service", Port: 8000, HealthPath: "/healthz"},
		},
		Primary: ha.Node{SystemID: primarySystem, Role: ha.RolePrimary, Address: "10.0.1.10"},
		Standby: ha.Node{SystemID: "sys-standby", Role: ha.RoleStandby, Address: "10.0.2.10"},
	}}
	svc := ha.NewService(store, nil, ha.Timings{ReportInterval: 30 * time.Second, StaleAfter: 90 * time.Second},
		func() string { return "0000000000000001" })
	h := NewHandler(Config{
		Agent: agent.NewService(newAgentStore(testRegistrationToken)),
		HA:    svc,
	})
	return h, store
}

func postHealth(h http.Handler, authorization, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/aiha/agent/health", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func healthBody(t *testing.T, req model.AgentHealthReq) string {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(b)
}

func TestAgentHealthNeedsAToken(t *testing.T) {
	h, store := newHealthHandler(t, "sys-0123456789abcdef")
	rec := postHealth(h, "", healthBody(t, model.AgentHealthReq{CollectedAt: time.Now()}))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(store.reporters) != 0 {
		t.Errorf("an unauthenticated report reached the store")
	}
}

func TestAgentHealthAssignsTheTokensSystem(t *testing.T) {
	h, store := newHealthHandler(t, "sys-0123456789abcdef")
	enrolled := enrol(t, h)

	rec := postHealth(h, "Bearer "+enrolled.AgentToken, healthBody(t, model.AgentHealthReq{CollectedAt: time.Now()}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var resp model.AgentHealthResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Assigned || resp.Assignment == nil || resp.Assignment.Role != "primary" || resp.ReportIntervalSeconds != 30 {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.Assignment.Service.Unit != "ai.service" || resp.Assignment.Resource.Device != "/dev/drbd0" {
		t.Errorf("assignment = %+v", resp.Assignment)
	}
	if len(store.reporters) != 1 || store.reporters[0] != enrolled.SystemID {
		t.Errorf("filed under %v, want %s", store.reporters, enrolled.SystemID)
	}
}

func TestAgentHealthUnassigned(t *testing.T) {
	h, _ := newHealthHandler(t, "sys-in-some-group")
	enrolled := enrol(t, h)

	rec := postHealth(h, "Bearer "+enrolled.AgentToken, healthBody(t, model.AgentHealthReq{CollectedAt: time.Now()}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"assigned":false`) || strings.Contains(rec.Body.String(), `"assignment"`) {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestAgentHealthRejects(t *testing.T) {
	h, store := newHealthHandler(t, "sys-0123456789abcdef")
	enrolled := enrol(t, h)
	auth := "Bearer " + enrolled.AgentToken

	cases := map[string]string{
		"malformed":          `{"collectedAt":`,
		"no collection time": `{}`,
		"unknown role":       healthBody(t, model.AgentHealthReq{CollectedAt: time.Now(), GroupID: "g", Role: "leader"}),
		"group without role": healthBody(t, model.AgentHealthReq{CollectedAt: time.Now(), GroupID: "g"}),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := postHealth(h, auth, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
		})
	}
	if len(store.reports) != 0 {
		t.Errorf("a rejected report reached the store: %+v", store.reports)
	}
}
