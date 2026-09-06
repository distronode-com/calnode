package db_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
)

// The row-level-security proof.
//
// ⛔ A SUPERUSER HANDLE PROVES NOTHING HERE. Superusers bypass row-level security
// unconditionally, and so does any role with BYPASSRLS, and so does the table's
// OWNER unless FORCE is set. The suite's DSN is normally the superuser that owns
// the test schema, so asserting isolation through it would pass whether the
// policies existed or not. Everything below therefore runs as a role created for
// the test with NOBYPASSRLS, which owns nothing, and the test SKIPS LOUDLY rather
// than falling back if such a role cannot be created or turns out to bypass
// anyway.
//
// The two halves are both required. The negative half (an unscoped read sees
// nothing, an unscoped write is refused) is what the policy is for. The positive
// half (a scoped read sees its own rows, a scoped write lands in its own
// workspace with no column named) is what stops the negative half from being
// satisfied by a database that simply does not work.

// appRole is a non-superuser application role with a handle opened as it.
type appRole struct {
	name   string
	handle *db.DB
}

// newAppRole creates a NOBYPASSRLS role, grants it the test schema's tables, and
// returns a handle connected as that role with search_path pointed at the schema.
//
// It reads the schema name back from the owner handle rather than taking it as an
// argument, because dbtest owns that name and nothing else exposes it.
func newAppRole(t *testing.T, owner *db.DB) *appRole {
	t.Helper()

	dsn := dbtest.PostgresDSN()
	if dsn == "" {
		t.Skipf("%s is not set; the row-level-security proof needs a real server", dbtest.DSNEnv)
	}

	var schema string
	if err := owner.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("read current_schema: %v", err)
	}

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	// The name is hex we generated, so interpolating it into DDL cannot carry a
	// quote or a keyword. PostgreSQL takes no placeholder for a role name.
	name := "calnode_app_" + hex.EncodeToString(buf)
	const password = "rls_proof_pw" // local test role, dropped at the end of the test

	if _, err := owner.Exec(`CREATE ROLE ` + name + ` LOGIN PASSWORD '` + password + `' NOBYPASSRLS`); err != nil {
		t.Skipf("LOUD SKIP: cannot CREATE ROLE on this server (%v) — the row-level-security proof "+
			"REQUIRES a NOBYPASSRLS role, and asserting isolation through the suite's own "+
			"superuser DSN would pass with or without the policies. Point %s at a server where "+
			"the test role may create roles.", err, dbtest.DSNEnv)
	}
	t.Cleanup(func() {
		// DROP OWNED also revokes the privileges granted below, which DROP ROLE
		// would otherwise refuse over.
		if _, err := owner.Exec(`DROP OWNED BY ` + name); err != nil {
			t.Errorf("drop owned by %s: %v", name, err)
		}
		if _, err := owner.Exec(`DROP ROLE ` + name); err != nil {
			t.Errorf("drop role %s: %v", name, err)
		}
	})

	for _, stmt := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + name,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + schema + ` TO ` + name,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA ` + schema + ` TO ` + name,
	} {
		if _, err := owner.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	handle, err := db.OpenDB(appDSN(t, dsn, name, password, schema))
	if err != nil {
		t.Fatalf("open as %s: %v", name, err)
	}
	t.Cleanup(func() { handle.Close() })

	// Belt and braces against the thing that makes this whole test vacuous.
	var super, bypass bool
	if err := handle.QueryRow(
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		t.Fatalf("read own role attributes: %v", err)
	}
	if super || bypass {
		t.Skipf("LOUD SKIP: the application role reports rolsuper=%v rolbypassrls=%v, so it BYPASSES "+
			"row-level security and every assertion below would pass vacuously", super, bypass)
	}
	if !ownsNothing(t, handle, schema) {
		t.Skipf("LOUD SKIP: the application role owns tables in %s, and a table's owner is exempt from "+
			"its own policy unless FORCE is set", schema)
	}

	return &appRole{name: name, handle: handle}
}

// ownsNothing reports whether the connected role owns none of the schema's
// tables. FORCE ROW LEVEL SECURITY covers the owner too, so this is belt and
// braces rather than the guarantee — but a role that owns the tables is not the
// role this test means to be.
func ownsNothing(t *testing.T, handle *db.DB, schema string) bool {
	t.Helper()
	var n int
	if err := handle.QueryRow(
		`SELECT COUNT(*) FROM pg_tables WHERE schemaname = ? AND tableowner = current_user`,
		schema).Scan(&n); err != nil {
		t.Fatalf("count owned tables: %v", err)
	}
	return n == 0
}

// appDSN rewrites the suite's DSN for the application role. pgx forwards
// unrecognised query parameters as runtime parameters, which is how search_path
// reaches the session — the same mechanism dbtest uses.
func appDSN(t *testing.T, dsn, user, password, schema string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", dbtest.DSNEnv, err)
	}
	u.User = url.UserPassword(user, password)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// TestPostgres_rlsIsolatesAnUnprivilegedRole is the gate on migration 00060 plus
// db.EnableRLS: with the policies live and no workspace bound, the application
// role can neither read nor write; with one bound, it reads and writes exactly
// its own workspace.
func TestPostgres_rlsIsolatesAnUnprivilegedRole(t *testing.T) {
	owner := dbtest.RequirePostgres(t)
	ctx := context.Background()

	seedTwoWorkspaces(t, owner)
	if err := owner.EnableRLS(ctx); err != nil {
		t.Fatalf("EnableRLS: %v", err)
	}

	app := newAppRole(t, owner)

	// Positive control on the DATA rather than the policy: the rows exist, so a
	// zero below is the policy hiding them and not an empty table.
	if got := ownerCount(t, owner, ""); got != 2 {
		t.Fatalf("the owner sees %d users; want 2 — the fixture did not land", got)
	}

	t.Run("unbound reads nothing", func(t *testing.T) {
		conn := pin(t, app.handle)
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("an unbound session sees %d of 2 users; want 0 — an unset app.workspace_id must match no row", n)
		}
	})

	t.Run("unbound write is refused and lands nowhere", func(t *testing.T) {
		conn := pin(t, app.handle)
		_, err := conn.ExecContext(ctx,
			`INSERT INTO users (id, email, name) VALUES ($1, $2, $3)`, "u-unbound", "unbound@example.com", "U")
		if err == nil {
			t.Fatal("an unbound INSERT succeeded; the policy's WITH CHECK must refuse it")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("unbound INSERT failed with %v; want SQLSTATE 42501 (insufficient_privilege, the RLS violation)", err)
		}
		// ⛔ The failure mode that matters: the column default is
		// COALESCE(current_setting(...), 'default'), so a policy that did not
		// refuse this would have written the row into the DEFAULT workspace.
		if got := ownerCount(t, owner, db.DefaultWorkspaceID); got != 0 {
			t.Errorf("%d users landed in the default workspace; the refused INSERT must leave nothing behind", got)
		}
		if got := ownerCount(t, owner, ""); got != 2 {
			t.Errorf("the owner now sees %d users; want the original 2", got)
		}
	})

	t.Run("bound reads and writes exactly one workspace", func(t *testing.T) {
		conn := pin(t, app.handle)
		if _, err := conn.ExecContext(ctx, `SELECT set_config('app.workspace_id', $1, false)`, "ws-a"); err != nil {
			t.Fatalf("bind workspace: %v", err)
		}

		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Errorf("a session bound to ws-a sees %d users; want 1", n)
		}

		var email string
		if err := conn.QueryRowContext(ctx, `SELECT email FROM users`).Scan(&email); err != nil {
			t.Fatalf("read: %v", err)
		}
		if email != "a@example.com" {
			t.Errorf("bound to ws-a, read %q; want a@example.com", email)
		}

		// No column named, which is D1's whole point: the ~200 INSERT statements
		// in the tree need no edit.
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO users (id, email, name) VALUES ($1, $2, $3)`, "u-a2", "a2@example.com", "A2"); err != nil {
			t.Fatalf("bound INSERT: %v", err)
		}
		var landed string
		if err := conn.QueryRowContext(ctx,
			`SELECT workspace_id FROM users WHERE id = $1`, "u-a2").Scan(&landed); err != nil {
			t.Fatalf("read back the inserted row: %v", err)
		}
		if landed != "ws-a" {
			t.Errorf("the inserted row landed in %q; want ws-a", landed)
		}

		// And B is still invisible, by the value the fixture put there.
		var visible int
		if err := conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM users WHERE email = $1`, "b@example.com").Scan(&visible); err != nil {
			t.Fatalf("count B's user: %v", err)
		}
		if visible != 0 {
			t.Errorf("bound to ws-a, B's user is visible")
		}
	})

	t.Run("naming another workspace explicitly is refused", func(t *testing.T) {
		conn := pin(t, app.handle)
		if _, err := conn.ExecContext(ctx, `SELECT set_config('app.workspace_id', $1, false)`, "ws-a"); err != nil {
			t.Fatalf("bind workspace: %v", err)
		}
		_, err := conn.ExecContext(ctx,
			`INSERT INTO users (id, email, name, workspace_id) VALUES ($1, $2, $3, $4)`,
			"u-cross", "cross@example.com", "X", "ws-b")
		if err == nil {
			t.Fatal("writing into another workspace succeeded; WITH CHECK must refuse it")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("cross-workspace INSERT failed with %v; want SQLSTATE 42501", err)
		}
	})
}

// seedTwoWorkspaces writes one workspace and one user each for ws-a and ws-b
// through the owner handle, naming workspace_id explicitly because the owner
// binds nothing.
func seedTwoWorkspaces(t *testing.T, owner *db.DB) {
	t.Helper()
	for _, ws := range []string{"ws-a", "ws-b"} {
		if _, err := owner.Exec(
			`INSERT INTO workspaces (id, slug, public_host, region, status) VALUES (?, ?, ?, '', 'active')`,
			ws, ws, ws+".example.com"); err != nil {
			t.Fatalf("seed workspace %s: %v", ws, err)
		}
	}
	for _, row := range []struct{ id, email, ws string }{
		{"u-a", "a@example.com", "ws-a"},
		{"u-b", "b@example.com", "ws-b"},
	} {
		if _, err := owner.Exec(
			`INSERT INTO users (id, email, name, workspace_id) VALUES (?, ?, ?, ?)`,
			row.id, row.email, row.id, row.ws); err != nil {
			t.Fatalf("seed user %s: %v", row.id, err)
		}
	}
}

// ownerCount counts users through the owner handle, which is exempt from the
// policy, optionally narrowed to one workspace. An empty workspace means all.
func ownerCount(t *testing.T, owner *db.DB, workspace string) int {
	t.Helper()
	var n int
	var err error
	if workspace == "" {
		err = owner.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	} else {
		err = owner.QueryRow(`SELECT COUNT(*) FROM users WHERE workspace_id = ?`, workspace).Scan(&n)
	}
	if err != nil {
		t.Fatalf("count through the owner handle: %v", err)
	}
	return n
}

// pin takes one connection out of the pool and keeps it for the caller.
//
// set_config(..., false) is SESSION scoped, so the statements that depend on it
// have to run on the same connection. Statements issued through a *sql.Conn are
// NOT rebound by the wrapper, so everything here is written in PostgreSQL's own
// $n form.
func pin(t *testing.T, handle *db.DB) *sql.Conn {
	t.Helper()
	conn, err := handle.Conn(context.Background())
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}
