package inventory

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests exercise the SQL against a real PostgreSQL, because what matters
// is a property of the statement: that a system keeps exactly one inventory
// and that every report moves reported_at forward, including one that changes
// nothing.

func testStore(t *testing.T) (*PGStore, *pgxpool.Pool) {
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
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM system_inventories WHERE system_id = $1`, testSystemID)
	})
	return NewPGStore(pool), pool
}

func rowCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM system_inventories WHERE system_id = $1`, testSystemID).Scan(&n)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestPGStoreGetWithoutAReport(t *testing.T) {
	store, _ := testStore(t)

	if _, err := store.Get(context.Background(), testSystemID); !errors.Is(err, ErrNoInventory) {
		t.Fatalf("err = %v, want ErrNoInventory", err)
	}
}

func TestPGStorePutAndGet(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	inv := healthy()

	if err := store.Put(ctx, testSystemID, testAgentID, inv); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, testSystemID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.AgentID != testAgentID || !got.Complete {
		t.Errorf("stored row = %+v, want agent %q and complete", got, testAgentID)
	}
	if len(got.Inventory.Partitions) != len(inv.Partitions) {
		t.Errorf("partitions = %d, want %d", len(got.Inventory.Partitions), len(inv.Partitions))
	}
	if got.Inventory.Partitions[0].Device != "/dev/nvme0n1p1" {
		t.Errorf("first partition = %+v", got.Inventory.Partitions[0])
	}
	if !got.CollectedAt.Truncate(time.Second).Equal(inv.CollectedAt.Truncate(time.Second)) {
		t.Errorf("CollectedAt = %s, want %s", got.CollectedAt, inv.CollectedAt)
	}
	if got.Now.Before(got.ReportedAt) {
		t.Errorf("the store's clock (%s) is behind reported_at (%s)", got.Now, got.ReportedAt)
	}
}

// A system keeps one inventory. A report that changes nothing still records
// that the framework heard it: that is what makes the row trustworthy.
func TestPGStoreRepeatedReportReplacesAndRefreshes(t *testing.T) {
	store, pool := testStore(t)
	ctx := context.Background()

	if err := store.Put(ctx, testSystemID, testAgentID, healthy()); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	first, err := store.Get(ctx, testSystemID)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}

	// Same content, reported again a moment later.
	time.Sleep(10 * time.Millisecond)
	if err := store.Put(ctx, testSystemID, testAgentID, healthy()); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	second, err := store.Get(ctx, testSystemID)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}

	if n := rowCount(t, pool); n != 1 {
		t.Errorf("rows = %d, want exactly one inventory per system", n)
	}
	if !second.ReportedAt.After(first.ReportedAt) {
		t.Errorf("reported_at did not move: %s then %s", first.ReportedAt, second.ReportedAt)
	}
}

func TestPGStoreReplacesTheWholeInventory(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()

	if err := store.Put(ctx, testSystemID, testAgentID, healthy()); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// The ESP has been unmounted since. The new report must not leave it
	// behind: a merged inventory would keep offering a partition that is gone.
	shrunk := healthy()
	shrunk.Partitions = shrunk.Partitions[:1]
	shrunk.Complete = false
	shrunk.IncompleteReason = "fstab declares /boot/efi, which is not mounted"
	if err := store.Put(ctx, testSystemID, "agent-second-install", shrunk); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	got, err := store.Get(ctx, testSystemID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Inventory.Partitions) != 1 {
		t.Errorf("partitions = %d, want the replacement's 1", len(got.Inventory.Partitions))
	}
	if got.Complete {
		t.Error("complete stayed true after an incomplete report")
	}
	if got.AgentID != "agent-second-install" {
		t.Errorf("AgentID = %q, want the reporting agent", got.AgentID)
	}
}

// The service refuses to choose targets from a stored row it did not like,
// not only from one it just received.
func TestServiceOverPGStore(t *testing.T) {
	store, _ := testStore(t)
	svc := NewService(store)
	ctx := context.Background()

	if err := svc.Report(ctx, testSystemID, testAgentID, healthy()); err != nil {
		t.Fatalf("Report: %v", err)
	}
	devices, expected, _, err := svc.BackupDevices(ctx, testSystemID)
	if err != nil {
		t.Fatalf("BackupDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Errorf("devices = %v, want two", devices)
	}
	if expected["/dev/nvme0n1p1"] != "uuid-root" {
		t.Errorf("expected UUIDs = %v", expected)
	}
}
