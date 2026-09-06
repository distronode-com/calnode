package db_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
)

// Boundary 2: the two handles, and the per-statement tenant binding.
//
// Everything that matters here needs a NOBYPASSRLS application role, for the
// reason rls_proof_test.go spells out at length: through a superuser handle these
// assertions pass whether the binding works or not. The harness is shared with
// that file.

// tenantPair is a live OpenPair against the test schema, with the application
// handle connected as a NOBYPASSRLS role that owns nothing.
type tenantPair struct {
	app      *db.DB // DATABASE_URL — the application role
	platform *db.DB // DATABASE_ADMIN_URL — the owner, which bypasses
	owner    *db.DB // the suite's own migrated handle, for fixtures and control reads
}

func openTenantPair(t *testing.T) tenantPair {
	t.Helper()

	owner := dbtest.RequirePostgres(t)
	seedTwoWorkspaces(t, owner)
	if err := owner.EnableRLS(context.Background()); err != nil {
		t.Fatalf("EnableRLS: %v", err)
	}

	role := newAppRole(t, owner) // skips loudly if it cannot be non-superuser
	role.handle.Close()          // OpenPair opens its own; this one was only the probe

	// The platform DSN is the suite's own, pinned to the test's schema. In this
	// harness that role is the superuser that owns the schema, which is exactly
	// what DATABASE_ADMIN_URL is in production: owner plus BYPASSRLS.
	platformDSN := withSchema(t, dbtest.PostgresDSN(), role.schema)

	app, platform, err := db.OpenPair(role.dsn, platformDSN)
	if err != nil {
		t.Fatalf("OpenPair: %v", err)
	}
	t.Cleanup(func() {
		app.Close()
		platform.Close()
	})

	// The guard that makes the '' binding on the platform handle safe, and that
	// would catch an application role able to read everything.
	if err := app.VerifyRoles(context.Background()); err != nil {
		t.Fatalf("VerifyRoles: %v", err)
	}

	return tenantPair{app: app, platform: platform, owner: owner}
}

func withSchema(t *testing.T, dsn, schema string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", dbtest.DSNEnv, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// TestOpenPair_workspaceHandleSeesOnlyItsOwn walks every statement shape a
// workspace handle offers. Each one is asserted twice: A sees its own row, and A
// does not see B's.
func TestOpenPair_workspaceHandleSeesOnlyItsOwn(t *testing.T) {
	p := openTenantPair(t)
	a := p.app.ForWorkspace("ws-a")

	if got := a.Workspace(); got != "ws-a" {
		t.Errorf("Workspace() = %q; want ws-a", got)
	}

	t.Run("QueryRow", func(t *testing.T) {
		var n int
		if err := a.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n != 1 {
			t.Errorf("count = %d; want 1 of the 2 users", n)
		}
		var email string
		err := a.QueryRow(`SELECT email FROM users WHERE id = ?`, "u-b").Scan(&email)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("reading B's user gave (%q, %v); want sql.ErrNoRows", email, err)
		}
		assertIdle(t, a, "after two QueryRow/Scan pairs")
	})

	t.Run("Query", func(t *testing.T) {
		rows, err := a.Query(`SELECT id FROM users ORDER BY id`)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		// The positive control on the release assertion: while the cursor is open
		// the connection IS held, so the zero after Close means something.
		if inUse := a.Stats().InUse; inUse != 1 {
			t.Errorf("with a cursor open the pool reports InUse=%d; want 1", inUse)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("rows.Close: %v", err)
		}
		if len(ids) != 1 || ids[0] != "u-a" {
			t.Errorf("ids = %v; want [u-a]", ids)
		}
		assertIdle(t, a, "after Rows.Close")
	})

	t.Run("Exec", func(t *testing.T) {
		// An UPDATE with no workspace predicate. It must reach A's row and not B's.
		res, err := a.Exec(`UPDATE users SET name = ?`, "renamed")
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			t.Fatalf("rows affected: %v", err)
		}
		if n != 1 {
			t.Errorf("an unscoped UPDATE touched %d rows; want 1", n)
		}
		var bName string
		if err := p.owner.QueryRow(`SELECT name FROM users WHERE id = ?`, "u-b").Scan(&bName); err != nil {
			t.Fatalf("read B through the owner: %v", err)
		}
		if bName == "renamed" {
			t.Error("the UPDATE reached B's row")
		}
		assertIdle(t, a, "after Exec")
	})

	t.Run("Tx", func(t *testing.T) {
		tx, err := a.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback() //nolint:errcheck // the standard pattern; release must be idempotent

		if inUse := a.Stats().InUse; inUse != 1 {
			t.Errorf("with a transaction open the pool reports InUse=%d; want 1", inUse)
		}
		if got := tx.Workspace(); got != "ws-a" {
			t.Errorf("tx.Workspace() = %q; want ws-a", got)
		}

		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
			t.Fatalf("count in tx: %v", err)
		}
		if n != 1 {
			t.Errorf("inside a transaction, count = %d; want 1", n)
		}
		if _, err := tx.Exec(`INSERT INTO teams (id, name, slug) VALUES (?, ?, ?)`, "t-a", "A", "a"); err != nil {
			t.Fatalf("insert in tx: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		assertIdle(t, a, "after Commit (and the deferred Rollback that follows)")

		var ws string
		if err := p.owner.QueryRow(`SELECT workspace_id FROM teams WHERE id = ?`, "t-a").Scan(&ws); err != nil {
			t.Fatalf("read the team through the owner: %v", err)
		}
		if ws != "ws-a" {
			t.Errorf("the team inserted in the transaction landed in %q; want ws-a", ws)
		}
	})
}

// TestOpenPair_insertNamesNoColumn is D1's payoff: the ~200 INSERT statements in
// the tree do not mention workspace_id and must not have to.
func TestOpenPair_insertNamesNoColumn(t *testing.T) {
	p := openTenantPair(t)
	a := p.app.ForWorkspace("ws-a")

	if _, err := a.Exec(
		`INSERT INTO users (id, email, name) VALUES (?, ?, ?)`,
		"u-a2", "a2@example.com", "A2"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var ws string
	if err := p.owner.QueryRow(`SELECT workspace_id FROM users WHERE id = ?`, "u-a2").Scan(&ws); err != nil {
		t.Fatalf("read back through the owner: %v", err)
	}
	if ws != "ws-a" {
		t.Errorf("the row landed in %q; want ws-a", ws)
	}
	assertIdle(t, a, "after Exec")
}

// TestOpenPair_insertIntoAnotherWorkspaceIsRefused: the WITH CHECK half. A
// handle bound to A cannot write into B even by naming it.
func TestOpenPair_insertIntoAnotherWorkspaceIsRefused(t *testing.T) {
	p := openTenantPair(t)
	a := p.app.ForWorkspace("ws-a")

	_, err := a.Exec(
		`INSERT INTO users (id, email, name, workspace_id) VALUES (?, ?, ?, ?)`,
		"u-cross", "cross@example.com", "X", "ws-b")
	if err == nil {
		t.Fatal("writing into ws-b through A's handle succeeded")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("error was %v; want SQLSTATE 42501 (the row-level-security violation)", err)
	}
	var n int
	if err := p.owner.QueryRow(`SELECT COUNT(*) FROM users WHERE id = ?`, "u-cross").Scan(&n); err != nil {
		t.Fatalf("count through the owner: %v", err)
	}
	if n != 0 {
		t.Error("the refused row is in the table")
	}
	assertIdle(t, a, "after a refused Exec")
}

// TestOpenPair_platformHandleSeesBoth. It is the handle the worker, the
// reconciler and every credential lookup run on, and those are meaningless if it
// cannot read across workspaces.
func TestOpenPair_platformHandleSeesBoth(t *testing.T) {
	p := openTenantPair(t)

	var n int
	if err := p.platform.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("the platform handle sees %d users; want both", n)
	}
	if got := p.platform.Workspace(); got != "" {
		t.Errorf("platform Workspace() = %q; want empty", got)
	}
	// It binds '' anyway, which is what keeps "every statement on a paired handle
	// sets the parameter" free of exceptions.
	if !p.platform.MultiTenant() {
		t.Error("the platform handle should still bind, even though it bypasses")
	}
	assertIdle(t, p.platform, "after QueryRow on the platform handle")

	// And Platform() on the app handle is that same handle, so nothing downstream
	// has to carry both.
	if p.app.Platform() != p.platform {
		t.Error("app.Platform() is not the platform handle OpenPair returned")
	}
	if p.app.ForWorkspace("ws-a").Platform() != p.platform {
		t.Error("a workspace handle lost its platform handle")
	}
}

// TestOpenPair_unboundAppHandleMatchesNothing: the pair's base app handle is not
// a back door. It binds ”, which no workspace id can equal.
func TestOpenPair_unboundAppHandleMatchesNothing(t *testing.T) {
	p := openTenantPair(t)

	var n int
	if err := p.app.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("the unbound application handle sees %d users; want 0", n)
	}
}

// TestOpenPair_handleSurvivesItsRequest is why the binding is per statement
// rather than a pinned connection: Calnode's handlers spawn fire-and-forget
// goroutines (notify hosts, enqueue the webhook, enqueue reminders) that outlive
// the request. A handle that pinned a session would be unusable there.
func TestOpenPair_handleSurvivesItsRequest(t *testing.T) {
	p := openTenantPair(t)

	// The "request": builds the handle, returns, goes out of scope.
	handle := func() *db.DB { return p.app.ForWorkspace("ws-a") }()

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var email string
			if err := handle.QueryRow(`SELECT email FROM users`).Scan(&email); err != nil {
				errs <- err
				return
			}
			if email != "a@example.com" {
				errs <- errors.New("goroutine read " + email)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	assertIdle(t, handle, "after four concurrent goroutine reads")
}

// TestForWorkspace_rejectsAMalformedID. The id is a bind parameter, never SQL
// text, so this is not an injection defence — it is a guard against a caller
// passing something that is not an id and getting a handle that silently matches
// nothing.
func TestForWorkspace_rejectsAMalformedID(t *testing.T) {
	p := openTenantPair(t)

	for _, bad := range []string{
		"", "WS-A", "ws a", "ws'a", "ws;a", "../etc", "a@b.com",
		"0123456789012345678901234567890123456789012345678901234567890123456789", // 70 chars
	} {
		h := p.app.ForWorkspace(bad)
		if h.Err() == nil {
			t.Errorf("ForWorkspace(%q) produced a usable handle", bad)
			continue
		}
		if !errors.Is(h.Err(), db.ErrInvalidWorkspace) {
			t.Errorf("ForWorkspace(%q) err = %v; want ErrInvalidWorkspace", bad, h.Err())
		}
		// Every statement shape has to refuse, not just report.
		var n int
		if err := h.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); !errors.Is(err, db.ErrInvalidWorkspace) {
			t.Errorf("QueryRow on a poisoned handle gave %v", err)
		}
		if _, err := h.Query(`SELECT 1`); !errors.Is(err, db.ErrInvalidWorkspace) {
			t.Errorf("Query on a poisoned handle gave %v", err)
		}
		if _, err := h.Exec(`SELECT 1`); !errors.Is(err, db.ErrInvalidWorkspace) {
			t.Errorf("Exec on a poisoned handle gave %v", err)
		}
		if _, err := h.Begin(); !errors.Is(err, db.ErrInvalidWorkspace) {
			t.Errorf("Begin on a poisoned handle gave %v", err)
		}
	}

	for _, good := range []string{"default", "ws-a", "a", "acme_corp", "ws-0123"} {
		if h := p.app.ForWorkspace(good); h.Err() != nil {
			t.Errorf("ForWorkspace(%q) = %v; want a usable handle", good, h.Err())
		}
	}
	assertIdle(t, p.app, "no statement should have reached the pool")
}

// TestOpenPair_prepareIsRefusedOnABoundHandle. A *sql.Stmt is re-prepared on
// whatever connection the pool hands it, with no hook to bind the tenant first,
// so it would run unbound and silently see nothing. Refusing is the only safe
// answer; nothing in the tree prepares a statement.
func TestOpenPair_prepareIsRefusedOnABoundHandle(t *testing.T) {
	p := openTenantPair(t)
	if _, err := p.app.ForWorkspace("ws-a").Prepare(`SELECT 1`); err == nil {
		t.Fatal("Prepare on a tenant-bound handle succeeded")
	}
}

// TestOpenPair_refusesSQLite: the library refuses the combination its caller is
// already supposed to have refused, because a library that only works when its
// caller validated is one that will be called by something else one day.
func TestOpenPair_refusesSQLite(t *testing.T) {
	if _, _, err := db.OpenPair("sqlite://:memory:", "postgres://x:y@h/db"); err == nil {
		t.Error("OpenPair accepted a SQLite application DSN")
	}
	if _, _, err := db.OpenPair("postgres://x:y@h/db", "sqlite://:memory:"); err == nil {
		t.Error("OpenPair accepted a SQLite platform DSN")
	}
}

// TestSingleHandle_forWorkspaceIsIdentity is the byte-identical promise at the
// handle layer. On SQLite and on single-tenant PostgreSQL, ForWorkspace returns
// the same handle, Platform returns the same handle, and no statement acquires a
// connection of its own or sets a session parameter — so nothing about an
// existing deployment changes.
func TestSingleHandle_forWorkspaceIsIdentity(t *testing.T) {
	handle := dbtest.Open(t)

	if handle.MultiTenant() {
		t.Error("a handle from OpenDB should not bind a tenant")
	}
	if handle.ForWorkspace("ws-a") != handle {
		t.Error("ForWorkspace returned a different handle on a single handle")
	}
	// Not even a malformed id, because there is nothing to validate against: one
	// workspace, no parameter to bind.
	if handle.ForWorkspace("NOT AN ID") != handle {
		t.Error("ForWorkspace validated on a single handle; it should be the identity function")
	}
	if handle.Platform() != handle {
		t.Error("Platform() should be the handle itself when there is one role")
	}
	if got := handle.Workspace(); got != "" {
		t.Errorf("Workspace() = %q; want empty", got)
	}
	// Prepare stays available, which it is on every existing deployment.
	stmt, err := handle.Prepare(`SELECT COUNT(*) FROM users`)
	if err != nil {
		t.Fatalf("Prepare on a single handle: %v", err)
	}
	stmt.Close()

	if dbtest.PostgresDSN() == "" {
		t.Log("SQLite: the tenant-binding cases in this file are skipped — there is no row-level " +
			"security to enforce isolation with, which is why config.Validate refuses MULTI_TENANT " +
			"without a postgres:// DSN. ForWorkspace being the identity function IS the SQLite behaviour.")
	}
}

// TestSingleHandle_verifyRolesIsANoOp: boot code needs no dialect or mode branch.
func TestSingleHandle_verifyRolesIsANoOp(t *testing.T) {
	if err := dbtest.Open(t).VerifyRoles(context.Background()); err != nil {
		t.Errorf("VerifyRoles on a single handle: %v", err)
	}
}

// assertIdle is the release proof. Every statement shape hands its connection
// back, so the pool returns to zero in use; a shape that leaked would show up
// here as a number that only ever grows.
func assertIdle(t *testing.T, handle *db.DB, when string) {
	t.Helper()
	if inUse := handle.Stats().InUse; inUse != 0 {
		t.Errorf("pool reports InUse=%d %s; the connection was not released", inUse, when)
	}
}
