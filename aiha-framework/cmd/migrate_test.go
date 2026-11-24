package cmd

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Every command applies every schema while the server may be serving agents.
// On a database that is up to date, applying them must wait for no lock that
// the server's writes hold. "CREATE INDEX IF NOT EXISTS" and "ALTER TABLE ...
// IF NOT EXISTS" lock their table before they find there is nothing to do;
// run as one transaction per schema, those locks met a claim's in the other
// order and PostgreSQL ended one of the two as a deadlock.
//
// Holding ROW EXCLUSIVE on every table is enough to catch them: it is what
// the server's writes take, and it conflicts with every lock stronger than a
// write's, including those a read would be blocked by.
func TestSchemasWaitForNoLockOnAnUpToDateDatabase(t *testing.T) {
	dsn := os.Getenv("AIHA_TEST_DB_DSN")
	if dsn == "" {
		t.Skip("AIHA_TEST_DB_DSN not set")
	}
	ctx := context.Background()
	writer, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	for _, migrate := range migrations {
		if err := migrate(ctx, writer); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Any wait at all fails the statement rather than the test timing out.
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "1s"
	impatient, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(impatient.Close)

	rows, err := writer.Query(ctx, `SELECT quote_ident(tablename) FROM pg_tables WHERE schemaname = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"repositories", "restore_jobs", "agents", "jobs", "attempts", "system_inventories", "ha_groups", "ha_nodes", "ha_flows"} {
		if !strings.Contains(" "+strings.Join(tables, " ")+" ", " "+want+" ") {
			t.Fatalf("table %s is missing after migrating: %v", want, tables)
		}
	}

	tx, err := writer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE `+strings.Join(tables, ", ")+` IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	for _, migrate := range migrations {
		if err := migrate(ctx, impatient); err != nil {
			t.Fatalf("migrate beside the server's writes: %v", err)
		}
	}
}
