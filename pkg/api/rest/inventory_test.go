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
	"github.com/cloud-barista/ai-ha/pkg/framework/inventory"
)

// inventoryStore records what the handler asked to store, so that a test can
// see which system a report was filed under. The rules themselves are covered
// in package inventory; what is under test here is the HTTP layer.
type inventoryStore struct {
	systemID string
	agentID  string
	inv      inventory.Inventory
	puts     int
}

func (s *inventoryStore) Put(_ context.Context, systemID, agentID string, inv inventory.Inventory) error {
	s.puts++
	s.systemID, s.agentID, s.inv = systemID, agentID, inv
	return nil
}

func (s *inventoryStore) Get(_ context.Context, systemID string) (inventory.Stored, error) {
	if s.puts == 0 || s.systemID != systemID {
		return inventory.Stored{}, inventory.ErrNoInventory
	}
	now := time.Now().UTC()
	return inventory.Stored{
		SystemID: s.systemID, AgentID: s.agentID, Inventory: s.inv,
		CollectedAt: s.inv.CollectedAt, ReportedAt: now, Complete: s.inv.Complete, Now: now,
	}, nil
}

func newInventoryHandler(t *testing.T) (http.Handler, *agentStore, *inventoryStore) {
	t.Helper()
	agents := newAgentStore(testRegistrationToken)
	store := &inventoryStore{}
	h := NewHandler(Config{
		Agent:     agent.NewService(agents),
		Inventory: inventory.NewService(store),
	})
	return h, agents, store
}

// enrol registers an agent and returns its token.
func enrol(t *testing.T, h http.Handler) model.AgentRegisterResp {
	t.Helper()
	rec := postRegister(h, registerBody(testRegistrationKey))
	if rec.Code != http.StatusOK {
		t.Fatalf("register: status %d: %s", rec.Code, rec.Body)
	}
	var resp model.AgentRegisterResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	return resp
}

func postInventory(h http.Handler, authorization, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/aiha/agent/inventory", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func inventoryBody(t *testing.T, mutate func(*model.AgentInventoryReq)) string {
	t.Helper()
	req := model.AgentInventoryReq{
		CollectedAt: time.Now().UTC(),
		Firmware:    "uefi",
		Complete:    true,
		Partitions: []model.InventoryPartition{
			{Device: "/dev/nvme0n1p1", MountPoint: "/", FSType: "ext4", Candidate: true, Selectable: true},
			{Device: "/dev/nvme0n1p16", MountPoint: "/boot/efi", FSType: "vfat", Candidate: true, Selectable: true},
		},
	}
	if mutate != nil {
		mutate(&req)
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("encode inventory: %v", err)
	}
	return string(b)
}

func TestInventoryReport(t *testing.T) {
	h, _, store := newInventoryHandler(t)
	reg := enrol(t, h)

	rec := postInventory(h, "Bearer "+reg.AgentToken, inventoryBody(t, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if store.puts != 1 {
		t.Fatalf("puts = %d, want 1", store.puts)
	}
	if len(store.inv.Partitions) != 2 {
		t.Errorf("partitions stored = %d, want 2", len(store.inv.Partitions))
	}
}

// The system a report is filed under comes from the token, never the body.
// Otherwise any valid agent token could describe another machine.
func TestInventoryIsFiledUnderTheTokensSystem(t *testing.T) {
	h, _, store := newInventoryHandler(t)
	reg := enrol(t, h)

	// A body that tries to name another system: the field does not exist on
	// the request type, so it can only arrive as unknown JSON.
	body := `{"systemId":"someone-elses-system","agentId":"someone-elses-agent",` +
		strings.TrimPrefix(inventoryBody(t, nil), "{")

	rec := postInventory(h, "Bearer "+reg.AgentToken, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if store.systemID != reg.SystemID {
		t.Errorf("stored under %q, want the token's system %q", store.systemID, reg.SystemID)
	}
	if store.agentID != reg.AgentID {
		t.Errorf("stored under agent %q, want the token's agent %q", store.agentID, reg.AgentID)
	}
}

func TestInventoryNeedsAnAgentToken(t *testing.T) {
	h, _, store := newInventoryHandler(t)
	body := inventoryBody(t, nil)

	for _, authorization := range []string{"", "Bearer ", "Basic abc", "Bearer unknown-token"} {
		rec := postInventory(h, authorization, body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("authorization %q: status %d, want 401", authorization, rec.Code)
		}
	}
	if store.puts != 0 {
		t.Error("an unauthenticated report was stored")
	}
}

func TestInventoryRejectsWhatCannotBeStored(t *testing.T) {
	h, _, store := newInventoryHandler(t)
	reg := enrol(t, h)

	for _, c := range []struct {
		name string
		body string
	}{
		{"malformed json", `{"partitions":`},
		{"no partitions", inventoryBody(t, func(r *model.AgentInventoryReq) { r.Partitions = nil })},
		{"no collection time", inventoryBody(t, func(r *model.AgentInventoryReq) { r.CollectedAt = time.Time{} })},
		{"a partition with no mount point", inventoryBody(t, func(r *model.AgentInventoryReq) {
			r.Partitions[0].MountPoint = ""
		})},
		{"a collection time in the future", inventoryBody(t, func(r *model.AgentInventoryReq) {
			r.CollectedAt = time.Now().UTC().Add(time.Hour)
		})},
		{"a duplicated device", inventoryBody(t, func(r *model.AgentInventoryReq) {
			r.Partitions[1].Device = r.Partitions[0].Device
		})},
		{"too many partitions", inventoryBody(t, func(r *model.AgentInventoryReq) {
			r.Partitions = nil
			for i := range 200 {
				r.Partitions = append(r.Partitions, model.InventoryPartition{
					MountPoint: "/mnt/" + string(rune('a'+i%26)),
				})
			}
		})},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := postInventory(h, "Bearer "+reg.AgentToken, c.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status %d, want 400: %s", rec.Code, rec.Body)
			}
		})
	}
	if store.puts != 0 {
		t.Errorf("puts = %d, want none: a rejected report reached the store", store.puts)
	}
}

// A report the agent marked incomplete is still recorded. Refusing it would
// leave the framework showing the last good inventory as if nothing had
// changed, which is worse than recording the doubt.
func TestInventoryRecordsAnIncompleteReport(t *testing.T) {
	h, _, store := newInventoryHandler(t)
	reg := enrol(t, h)

	body := inventoryBody(t, func(r *model.AgentInventoryReq) {
		r.Partitions = r.Partitions[:1]
		r.Complete = false
		r.Missing = []string{"/boot/efi"}
		r.IncompleteReason = "fstab declares /boot/efi, which is not mounted"
	})
	if rec := postInventory(h, "Bearer "+reg.AgentToken, body); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if store.inv.Complete {
		t.Error("an incomplete report was stored as complete")
	}
	if len(store.inv.Missing) != 1 || store.inv.Missing[0] != "/boot/efi" {
		t.Errorf("Missing = %v, want /boot/efi", store.inv.Missing)
	}
}

// Without an inventory service the route must not exist at all.
func TestInventoryRouteAbsentWithoutService(t *testing.T) {
	agents := newAgentStore(testRegistrationToken)
	h := NewHandler(Config{Agent: agent.NewService(agents)})
	reg := enrol(t, h)

	rec := postInventory(h, "Bearer "+reg.AgentToken, inventoryBody(t, nil))
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status %d: the inventory route was served without an inventory service", rec.Code)
	}
}

// Nothing in a report should carry a secret, and the response should not echo
// one back either.
func TestInventoryResponseCarriesNoSystemDetail(t *testing.T) {
	h, _, _ := newInventoryHandler(t)
	reg := enrol(t, h)

	rec := postInventory(h, "Bearer "+reg.AgentToken, inventoryBody(t, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	for _, leak := range []string{"/dev/nvme0n1p1", reg.AgentToken, reg.SystemID} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the response repeats %q back to the caller: %s", leak, rec.Body)
		}
	}
}
