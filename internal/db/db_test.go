package db_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/calnode/calnode/internal/db"
)

func TestOpen_inMemory(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()

	if err := database.Ping(); err != nil {
		t.Fatalf("db.Ping: %v", err)
	}
}

func TestMigrate_runsClean(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()

	if err := db.Migrate(database); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
}

func TestMigrate_idempotent(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()

	// Running twice should not error (goose is idempotent).
	for range 2 {
		if err := db.Migrate(database); err != nil {
			t.Fatalf("db.Migrate (run 2): %v", err)
		}
	}
}

func TestMigrate_tablesExist(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()

	if err := db.Migrate(database); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	tables := []string{
		"users", "api_keys", "teams", "team_members",
		"event_types", "event_type_questions",
		"availability_rules", "availability_overrides",
		"calendar_connections",
		"bookings", "booking_attendees", "booking_answers",
		"webhooks", "webhook_deliveries",
		"jobs",
	}

	for _, table := range tables {
		var name string
		err := database.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found after migration: %v", table, err)
		}
	}
}

func TestSchemaReady_falseBeforeMigrate_trueAfter(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()

	ctx := context.Background()

	// Before migrating, the goose bookkeeping table is absent → not ready.
	if ready, _ := db.SchemaReady(ctx, database); ready {
		t.Error("SchemaReady = true before migrations ran; want false")
	}

	if err := db.Migrate(database); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	ready, err := db.SchemaReady(ctx, database)
	if err != nil {
		t.Fatalf("SchemaReady after migrate: %v", err)
	}
	if !ready {
		t.Error("SchemaReady = false after migrations ran; want true")
	}

	// Applied version must equal the embedded target version.
	target, err := db.TargetVersion()
	if err != nil {
		t.Fatalf("TargetVersion: %v", err)
	}
	applied, err := db.AppliedVersion(ctx, database)
	if err != nil {
		t.Fatalf("AppliedVersion: %v", err)
	}
	if applied != target {
		t.Errorf("applied version = %d; want target %d", applied, target)
	}
	if target < 17 {
		t.Errorf("target version = %d; want >= 17 (sanity check against known migrations)", target)
	}
}

func TestDoubleBookingIndex_exists(t *testing.T) {
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()

	if err := db.Migrate(database); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	var name string
	err = database.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_bookings_no_double'`,
	).Scan(&name)
	if err != nil {
		t.Errorf("double-booking guard index not found: %v", err)
	}
}

// TestOpenDB_sqlitePragmasAndPool pins the SQLite path against accidental
// change: the single connection is a correctness guarantee (ARCHITECTURE §17),
// not a tuning choice, and the pragmas are connection-scoped so losing the
// connection loses them. A file database is used because :memory: cannot be in
// WAL mode.
func TestOpenDB_sqlitePragmasAndPool(t *testing.T) {
	handle, err := db.OpenDB("sqlite://" + filepath.Join(t.TempDir(), "calnode.db"))
	if err != nil {
		t.Fatalf("db.OpenDB: %v", err)
	}
	defer handle.Close()

	if got := handle.Dialect(); got != db.DialectSQLite {
		t.Errorf("dialect = %v; want %v", got, db.DialectSQLite)
	}
	if got := handle.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d; want 1", got)
	}

	pragmas := []struct{ name, want string }{
		{"journal_mode", "wal"},
		{"foreign_keys", "1"},
		{"busy_timeout", "5000"},
	}
	for _, p := range pragmas {
		var got string
		if err := handle.QueryRow(`PRAGMA ` + p.name).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", p.name, err)
		}
		if got != p.want {
			t.Errorf("PRAGMA %s = %q; want %q", p.name, got, p.want)
		}
	}
}

// TestOpenDB_wrapperRoundTrip exercises the method set the rest of the codebase
// uses, on the dialect where rebinding is a no-op, so a mistake in the wrapper
// itself cannot hide behind a missing Postgres server.
func TestOpenDB_wrapperRoundTrip(t *testing.T) {
	handle, err := db.OpenDB("sqlite://:memory:")
	if err != nil {
		t.Fatalf("db.OpenDB: %v", err)
	}
	defer handle.Close()

	if err := handle.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	ctx := context.Background()

	if _, err := handle.ExecContext(ctx,
		`INSERT INTO users (id, email, name) VALUES (?, ?, ?)`,
		"u1", "a@example.com", "A"); err != nil {
		t.Fatalf("ExecContext insert: %v", err)
	}

	var name string
	if err := handle.QueryRowContext(ctx,
		`SELECT name FROM users WHERE id = ?`, "u1").Scan(&name); err != nil {
		t.Fatalf("QueryRowContext: %v", err)
	}
	if name != "A" {
		t.Errorf("name = %q; want %q", name, "A")
	}

	rows, err := handle.QueryContext(ctx, `SELECT id FROM users WHERE email = ?`, "a@example.com")
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	count := 0
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	rows.Close()
	if count != 1 {
		t.Errorf("rows returned = %d; want 1", count)
	}

	tx, err := handle.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if tx.Dialect() != handle.Dialect() {
		t.Errorf("tx dialect = %v; want %v", tx.Dialect(), handle.Dialect())
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET name = ? WHERE id = ?`, "B", "u1"); err != nil {
		tx.Rollback()
		t.Fatalf("tx.ExecContext: %v", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT name FROM users WHERE id = ?`, "u1").Scan(&name); err != nil {
		tx.Rollback()
		t.Fatalf("tx.QueryRowContext: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("tx.Commit: %v", err)
	}
	if name != "B" {
		t.Errorf("name after tx update = %q; want %q", name, "B")
	}

	stmt, err := handle.PrepareContext(ctx, `SELECT COUNT(*) FROM users WHERE email = ?`)
	if err != nil {
		t.Fatalf("PrepareContext: %v", err)
	}
	defer stmt.Close()
	var n int
	if err := stmt.QueryRowContext(ctx, "a@example.com").Scan(&n); err != nil {
		t.Fatalf("stmt.QueryRowContext: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d; want 1", n)
	}
}
