package db_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
)

// These tests hold migration 00060: the tenant column, its foreign key, the
// per-table policy, and the classification of every table as tenant or exempt.
//
// The structural assertions read information_schema and pg_catalog rather than
// the migration file, so they describe what the database ended up with. What
// they deliberately do NOT prove is that the policies isolate anything: the test
// DSN is a superuser, and superusers bypass row-level security, so a behavioural
// assertion here would pass with or without the policies. That proof needs a
// NOBYPASSRLS application role, which arrives with the handle work in Boundary 2.

// TestTenancy_tableListsCoverTheSchema is the drift gate. A table added by a
// later migration has to be classified, so "nobody remembered to give it a
// workspace_id" cannot happen quietly.
func TestTenancy_tableListsCoverTheSchema(t *testing.T) {
	handle := dbtest.RequirePostgres(t)

	rows, err := handle.Query(
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'
		 ORDER BY table_name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	live := scanStrings(t, rows)

	if len(live) < 30 {
		t.Fatalf("only %d tables in the schema; the comparison would prove nothing", len(live))
	}

	classified := append(slices.Clone(db.TenantTables), db.ExemptTables...)
	slices.Sort(classified)

	for _, table := range live {
		if !slices.Contains(classified, table) {
			t.Errorf("table %q is in neither db.TenantTables nor db.ExemptTables — classify it (and give it a workspace_id if it is a tenant table)", table)
		}
	}
	for _, table := range classified {
		if !slices.Contains(live, table) {
			t.Errorf("table %q is classified in internal/db but does not exist in the migrated schema", table)
		}
	}
	if got, want := len(classified), len(live); got != want {
		t.Errorf("classified %d tables, schema has %d", got, want)
	}
	t.Logf("classified %d tables: %d tenant, %d exempt", len(live), len(db.TenantTables), len(db.ExemptTables))
}

// TestPostgres_tenantColumnShape asserts D1 for every tenant table at once: the
// column exists, is TEXT, is NOT NULL, and defaults from the session setting.
func TestPostgres_tenantColumnShape(t *testing.T) {
	handle := dbtest.RequirePostgres(t)

	rows, err := handle.Query(
		`SELECT table_name, data_type, is_nullable, COALESCE(column_default, '')
		 FROM information_schema.columns
		 WHERE table_schema = current_schema() AND column_name = 'workspace_id'
		 ORDER BY table_name`)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	defer rows.Close()

	seen := map[string]bool{}
	for rows.Next() {
		var table, dataType, nullable, def string
		if err := rows.Scan(&table, &dataType, &nullable, &def); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[table] = true

		if !slices.Contains(db.TenantTables, table) {
			t.Errorf("%s has a workspace_id column but is not in db.TenantTables", table)
		}
		if dataType != "text" {
			t.Errorf("%s.workspace_id is %s; want text", table, dataType)
		}
		if nullable != "NO" {
			t.Errorf("%s.workspace_id is nullable", table)
		}
		// The setting has to be named in the default, or a statement that omits
		// the column would land in the wrong tenant rather than being refused.
		if !strings.Contains(def, db.WorkspaceSetting) {
			t.Errorf("%s.workspace_id default %q does not read %s", table, def, db.WorkspaceSetting)
		}
		// ⚠️ The missing_ok form matters: the bare current_setting RAISES on an
		// unset parameter, which would fail every INSERT in single-tenant mode.
		if !strings.Contains(def, "true") {
			t.Errorf("%s.workspace_id default %q does not use the missing_ok form of current_setting", table, def)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	for _, table := range db.TenantTables {
		if !seen[table] {
			t.Errorf("tenant table %s has no workspace_id column", table)
		}
	}
	t.Logf("checked workspace_id on %d tables", len(seen))
}

// TestPostgres_exemptTablesHaveNoTenantColumn is the other half: an exempt table
// that quietly grew a workspace_id would be a table with a tenant column and no
// policy, which is worse than either.
func TestPostgres_exemptTablesHaveNoTenantColumn(t *testing.T) {
	handle := dbtest.RequirePostgres(t)

	for _, table := range db.ExemptTables {
		var n int
		err := handle.QueryRow(
			`SELECT COUNT(*) FROM information_schema.columns
			 WHERE table_schema = current_schema() AND table_name = ? AND column_name = 'workspace_id'`,
			table).Scan(&n)
		if err != nil {
			t.Fatalf("count columns of %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("exempt table %s has a workspace_id column", table)
		}
	}
}

// TestPostgres_tenantForeignKeys asserts the FK to workspaces(id) with ON DELETE
// CASCADE, which is what makes a workspace delete (D12) remove its rows rather
// than orphan them.
func TestPostgres_tenantForeignKeys(t *testing.T) {
	handle := dbtest.RequirePostgres(t)

	rows, err := handle.Query(
		`SELECT c.relname, con.confdeltype
		 FROM pg_constraint con
		 JOIN pg_class c ON c.oid = con.conrelid
		 JOIN pg_class f ON f.oid = con.confrelid
		 JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = con.conkey[1]
		 WHERE con.contype = 'f'
		   AND c.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema())
		   AND a.attname = 'workspace_id'
		   AND f.relname = 'workspaces'
		   AND array_length(con.conkey, 1) = 1
		 ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("read foreign keys: %v", err)
	}
	defer rows.Close()

	found := map[string]string{}
	for rows.Next() {
		var table, deleteRule string
		if err := rows.Scan(&table, &deleteRule); err != nil {
			t.Fatalf("scan: %v", err)
		}
		found[table] = deleteRule
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	for _, table := range db.TenantTables {
		rule, ok := found[table]
		if !ok {
			t.Errorf("%s.workspace_id has no foreign key to workspaces(id)", table)
			continue
		}
		if rule != "c" { // 'c' is CASCADE in pg_constraint.confdeltype
			t.Errorf("%s.workspace_id foreign key delete rule is %q; want CASCADE", table, rule)
		}
	}
	t.Logf("checked %d cascading foreign keys to workspaces(id)", len(found))
}

// TestPostgres_exactlyOnePolicyPerTenantTable asserts D2's policy half. Exactly
// one, because policies are permissive and OR together: a second one is a hole,
// not a redundancy.
func TestPostgres_exactlyOnePolicyPerTenantTable(t *testing.T) {
	handle := dbtest.RequirePostgres(t)

	rows, err := handle.Query(
		`SELECT tablename, policyname, cmd, COALESCE(qual, ''), COALESCE(with_check, '')
		 FROM pg_policies WHERE schemaname = current_schema()
		 ORDER BY tablename, policyname`)
	if err != nil {
		t.Fatalf("read pg_policies: %v", err)
	}
	defer rows.Close()

	type policy struct{ name, cmd, using, check string }
	byTable := map[string][]policy{}
	for rows.Next() {
		var table string
		var p policy
		if err := rows.Scan(&table, &p.name, &p.cmd, &p.using, &p.check); err != nil {
			t.Fatalf("scan: %v", err)
		}
		byTable[table] = append(byTable[table], p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	for _, table := range db.TenantTables {
		got := byTable[table]
		if len(got) != 1 {
			t.Errorf("%s has %d policies; want exactly 1 (permissive policies OR together, so a second one is a hole)", table, len(got))
			continue
		}
		p := got[0]
		if want := table + "_tenant"; p.name != want {
			t.Errorf("%s policy is named %q; want %q", table, p.name, want)
		}
		if p.cmd != "ALL" {
			t.Errorf("%s policy covers %q; want ALL", table, p.cmd)
		}
		for label, expr := range map[string]string{"USING": p.using, "WITH CHECK": p.check} {
			if !strings.Contains(expr, db.WorkspaceSetting) {
				t.Errorf("%s policy %s clause %q does not read %s", table, label, expr, db.WorkspaceSetting)
			}
		}
	}

	// workspaces carries a SELECT-only policy so the application role can resolve
	// a host and read a status without being able to write the tenant root.
	ws := byTable["workspaces"]
	if len(ws) != 1 {
		t.Fatalf("workspaces has %d policies; want exactly 1", len(ws))
	}
	if ws[0].cmd != "SELECT" {
		t.Errorf("workspaces policy covers %q; want SELECT", ws[0].cmd)
	}
	if ws[0].check != "" {
		t.Errorf("workspaces policy has a WITH CHECK (%q); a SELECT policy that could admit a write is not what was intended", ws[0].check)
	}

	for _, table := range []string{"crypto_keystore", "goose_db_version", "oauth_clients"} {
		if n := len(byTable[table]); n != 0 {
			t.Errorf("exempt table %s has %d policies; want 0", table, n)
		}
	}
}

// TestPostgres_rlsIsOffUntilEnabled is the single-tenant promise, held as an
// assertion rather than a comment: migrating alone must not change what an
// existing PostgreSQL deployment can see.
//
// ⛔ The reason this is not in the migration: FORCE ROW LEVEL SECURITY applies a
// policy to the table's OWNER too, and in single-tenant mode DATABASE_URL is the
// owner. Measured on PostgreSQL 17.11 with a NOBYPASSRLS owner role, a schema
// migrated with FORCE and no app.workspace_id binding returns 0 rows for every
// SELECT. A superuser DSN hides that completely.
func TestPostgres_rlsIsOffUntilEnabled(t *testing.T) {
	handle := dbtest.RequirePostgres(t)

	before := rlsFlags(t, handle)
	for _, table := range db.TenantTables {
		f, ok := before[table]
		if !ok {
			t.Fatalf("%s missing from pg_class", table)
		}
		if f.enabled || f.forced {
			t.Errorf("%s has row-level security enabled straight after migrating (enabled=%v forced=%v); "+
				"single-tenant PostgreSQL must be unaffected by 00060", table, f.enabled, f.forced)
		}
	}

	ctx := context.Background()
	if err := handle.EnableRLS(ctx); err != nil {
		t.Fatalf("EnableRLS: %v", err)
	}
	// Twice, because it runs on every boot.
	if err := handle.EnableRLS(ctx); err != nil {
		t.Fatalf("EnableRLS (second call): %v", err)
	}

	after := rlsFlags(t, handle)
	for _, table := range db.TenantTables {
		f := after[table]
		if !f.enabled || !f.forced {
			t.Errorf("%s after EnableRLS: enabled=%v forced=%v; want both true", table, f.enabled, f.forced)
		}
	}
	for _, table := range db.ExemptTables {
		if f := after[table]; f.enabled || f.forced {
			t.Errorf("exempt table %s had row-level security enabled (enabled=%v forced=%v)", table, f.enabled, f.forced)
		}
	}
	t.Logf("EnableRLS turned on %d tenant tables and left %d exempt ones alone", len(db.TenantTables), len(db.ExemptTables))
}

// TestSQLite_enableRLSIsANoOp: the same call on the engine that has no row-level
// security must not error, so boot code needs no dialect branch.
func TestSQLite_enableRLSIsANoOp(t *testing.T) {
	if dbtest.PostgresDSN() != "" {
		t.Skip("this case is about the SQLite path; the suite is pointed at PostgreSQL")
	}
	handle := dbtest.Open(t)
	if err := handle.EnableRLS(context.Background()); err != nil {
		t.Fatalf("EnableRLS on SQLite: %v", err)
	}
}

// TestPostgres_compositeUniqueness asserts D8 and D9: what became per-workspace,
// and what deliberately stayed global.
func TestPostgres_compositeUniqueness(t *testing.T) {
	handle := dbtest.RequirePostgres(t)

	rows, err := handle.Query(
		`SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = current_schema()`)
	if err != nil {
		t.Fatalf("read pg_indexes: %v", err)
	}
	defs := map[string]string{}
	func() {
		defer rows.Close()
		for rows.Next() {
			var name, def string
			if err := rows.Scan(&name, &def); err != nil {
				t.Fatalf("scan index: %v", err)
			}
			defs[name] = def
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate indexes: %v", err)
		}
	}()

	// Per workspace now. A UNIQUE constraint is backed by an index of the same
	// name, so both spellings are visible here.
	perWorkspace := map[string][]string{
		"users_workspace_id_email_key":      {"workspace_id", "email"},
		"event_types_workspace_id_slug_key": {"workspace_id", "slug"},
		"teams_workspace_id_slug_key":       {"workspace_id", "slug"},
		"idempotency_keys_pkey":             {"workspace_id", "idempotency_key"},
		"meeting_consents_pkey":             {"workspace_id", "room", "participant_identity"},
		"server_settings_pkey":              {"workspace_id", "id"},
		"ux_jobs_type_payload":              {"workspace_id", "type", "payload"},
		"idx_notes_booking":                 {"workspace_id", "booking_id"},
		"idx_bookings_no_double":            {"workspace_id", "host_id", "start_at"},
		"idx_bookings_host_time":            {"workspace_id", "host_id", "start_at", "end_at"},
		"idx_jobs_pending":                  {"workspace_id", "run_at"},
		"idx_jobs_running_expired":          {"workspace_id", "locked_until"},
		"idx_jobs_pending_global":           {"run_at"},
		"idx_jobs_running_expired_global":   {"locked_until"},
	}
	for name, cols := range perWorkspace {
		def, ok := defs[name]
		if !ok {
			t.Errorf("index %s does not exist", name)
			continue
		}
		if got := indexColumns(def); !slices.Equal(got, cols) {
			t.Errorf("index %s covers %v; want %v (%s)", name, got, cols, def)
		}
	}

	// The global ones went away.
	for _, gone := range []string{"users_email_key", "event_types_slug_key", "teams_slug_key"} {
		if def, ok := defs[gone]; ok {
			t.Errorf("%s still exists (%s); it should have become the (workspace_id, …) form", gone, def)
		}
	}

	// Credentials stay global: the tenant of a request that carries one is
	// resolved FROM it, so the lookup happens before any workspace is known.
	global := map[string][]string{
		"api_keys_key_hash_key":                {"key_hash"},
		"sessions_pkey":                        {"id"},
		"oauth_access_tokens_token_hash_key":   {"token_hash"},
		"oauth_access_tokens_refresh_hash_key": {"refresh_hash"},
		"oauth_auth_codes_pkey":                {"code_hash"},
		"booking_manage_tokens_pkey":           {"token_hash"},
		"magic_link_tokens_pkey":               {"token_hash"},
		"invite_tokens_token_hash_key":         {"token_hash"},
		"crypto_keystore_label_key":            {"label"},
	}
	for name, cols := range global {
		def, ok := defs[name]
		if !ok {
			t.Errorf("index %s does not exist", name)
			continue
		}
		if got := indexColumns(def); !slices.Equal(got, cols) {
			t.Errorf("index %s covers %v; want %v — this one has to stay global (%s)", name, got, cols, def)
		}
	}
}

// TestTenancy_defaultWorkspaceSeeded runs on whichever engine the environment
// selects, because the foreign keys on PostgreSQL and every existing row on both
// engines depend on that one row existing.
func TestTenancy_defaultWorkspaceSeeded(t *testing.T) {
	handle := dbtest.Open(t)

	var id, slug, host, status string
	err := handle.QueryRow(
		`SELECT id, slug, public_host, status FROM workspaces WHERE id = ?`,
		db.DefaultWorkspaceID).Scan(&id, &slug, &host, &status)
	if err != nil {
		t.Fatalf("read the default workspace: %v", err)
	}
	if slug != db.DefaultWorkspaceID {
		t.Errorf("slug = %q; want %q", slug, db.DefaultWorkspaceID)
	}
	// Empty on purpose: no HTTP request carries an empty Host, so host
	// resolution can never land on the default workspace.
	if host != "" {
		t.Errorf("public_host = %q; want empty", host)
	}
	if status != "active" {
		t.Errorf("status = %q; want active", status)
	}

	var n int
	if err := handle.QueryRow(`SELECT COUNT(*) FROM workspaces`).Scan(&n); err != nil {
		t.Fatalf("count workspaces: %v", err)
	}
	if n != 1 {
		t.Errorf("migrating seeded %d workspaces; want exactly 1", n)
	}
}

// TestTenancy_existingRowsLandInTheDefaultWorkspace: an INSERT that names no
// workspace_id has to work unchanged, on both engines, which is the whole point
// of D1's column default — the ~200 INSERT statements in the tree need no edit.
func TestTenancy_existingRowsLandInTheDefaultWorkspace(t *testing.T) {
	handle := dbtest.Open(t)

	if _, err := handle.Exec(
		`INSERT INTO users (id, email, name) VALUES (?, ?, ?)`,
		"u1", "a@example.com", "A"); err != nil {
		t.Fatalf("insert without naming workspace_id: %v", err)
	}

	var ws string
	if err := handle.QueryRow(`SELECT workspace_id FROM users WHERE id = ?`, "u1").Scan(&ws); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if ws != db.DefaultWorkspaceID {
		t.Errorf("workspace_id = %q; want %q", ws, db.DefaultWorkspaceID)
	}
}

type rlsFlag struct{ enabled, forced bool }

func rlsFlags(t *testing.T, handle *db.DB) map[string]rlsFlag {
	t.Helper()

	rows, err := handle.Query(
		`SELECT relname, relrowsecurity, relforcerowsecurity FROM pg_class
		 WHERE relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema())
		   AND relkind = 'r'`)
	if err != nil {
		t.Fatalf("read pg_class: %v", err)
	}
	defer rows.Close()

	out := map[string]rlsFlag{}
	for rows.Next() {
		var name string
		var f rlsFlag
		if err := rows.Scan(&name, &f.enabled, &f.forced); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[name] = f
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return out
}

// indexColumns pulls the column list out of a pg_indexes definition. It anchors
// on "USING btree (" rather than the last parenthesis, because a partial index's
// WHERE clause carries parentheses of its own and there are five of those here.
// Expression indexes would need more than this; none of the ones asserted above
// is one.
func indexColumns(def string) []string {
	const anchor = "USING btree ("
	start := strings.Index(def, anchor)
	if start < 0 {
		return nil
	}
	start += len(anchor)
	end := strings.IndexByte(def[start:], ')')
	if end < 0 {
		return nil
	}
	var out []string
	for _, part := range strings.Split(def[start:start+end], ",") {
		// Strip a trailing operator class, collation or sort direction.
		field := strings.TrimSpace(part)
		if i := strings.IndexByte(field, ' '); i >= 0 {
			field = field[:i]
		}
		out = append(out, field)
	}
	return out
}
