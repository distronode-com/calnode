package dbtest

import (
	"testing"

	"github.com/calnode/calnode/internal/db"
)

// TestSearchPathIsolation checks the mechanism openPostgres relies on, without
// going through it: that a search_path in the DSN really does land unqualified
// objects in the per-test schema, and that they are invisible from the default
// path. If pgx ever stopped forwarding the parameter, openPostgres would silently
// migrate into the public schema and two packages would fight over one set of
// tables — which reads as flaky tests, not as a broken harness.
func TestSearchPathIsolation(t *testing.T) {
	dsn := PostgresDSN()
	if dsn == "" {
		t.Skipf("%s is not set", DSNEnv)
	}

	schema := "calnode_test_" + randomSuffix(t)

	admin, err := db.OpenDB(dsn)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(`CREATE SCHEMA ` + quoteIdent(schema)); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	defer func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + quoteIdent(schema) + ` CASCADE`); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	}()

	scoped, err := db.OpenDB(withSearchPath(t, dsn, schema))
	if err != nil {
		t.Fatalf("open scoped: %v", err)
	}
	defer scoped.Close()

	var got string
	if err := scoped.QueryRow(`SELECT current_schema()`).Scan(&got); err != nil {
		t.Fatalf("current_schema: %v", err)
	}
	if got != schema {
		t.Fatalf("current_schema() = %q; want %q — search_path did not reach the server", got, schema)
	}

	if _, err := scoped.Exec(`CREATE TABLE isolated (id text)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	var visible int
	if err := admin.QueryRow(
		`SELECT COUNT(*) FROM pg_tables WHERE schemaname = current_schema() AND tablename = 'isolated'`).
		Scan(&visible); err != nil {
		t.Fatalf("count on default path: %v", err)
	}
	if visible != 0 {
		t.Errorf("table 'isolated' is visible on the default search_path; the schema is not isolating anything")
	}
}

// TestOpen returns a handle that has been migrated, on whichever engine is
// configured. goose's bookkeeping table is the engine-independent proof.
func TestOpen(t *testing.T) {
	h := Open(t)
	var n int
	if err := h.QueryRow(`SELECT COUNT(*) FROM goose_db_version`).Scan(&n); err != nil {
		t.Fatalf("goose_db_version: %v", err)
	}
	if n == 0 {
		t.Error("goose_db_version is empty; Open returned an unmigrated handle")
	}
}
