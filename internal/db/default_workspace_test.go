package db_test

import (
	"context"
	"testing"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
)

// The seeded `default` workspace on a MULTI_TENANT instance.
//
// Migration 00060 seeds it because it is the workspace every single-tenant row belongs to
// and the one the SQLite column default names. On a multi-tenant instance it is a tenant
// nobody owns: no public_host (so no Host resolves to it), no users, no settings. Left
// active, every background sweep still enumerates it — activeWorkspaceIDs filters on
// status = 'active' — so the instance keeps doing work on behalf of a tenant that cannot
// receive it.
//
// ⛔ The decision: suspend it at multi-tenant BOOT, not in the migration. A migration
// cannot see MULTI_TENANT, and in single-tenant mode `default` IS the workspace, so a
// suspended row there would make Scoped answer 503 to every request on the instance. One
// status flip removes it from every sweep at once, reusing D12's suspended semantics
// rather than inventing a second exclusion rule that a new loop could forget.
func TestPostgres_defaultWorkspaceIsSuspendedAtMultiTenantBoot(t *testing.T) {
	handle := dbtest.RequirePostgres(t)
	ctx := context.Background()

	// Freshly migrated, it is active — which is what single-tenant needs.
	var status string
	if err := handle.QueryRow(
		`SELECT status FROM workspaces WHERE id = ?`, db.DefaultWorkspaceID).Scan(&status); err != nil {
		t.Fatalf("read the default workspace: %v", err)
	}
	if status != "active" {
		t.Fatalf("straight after migrating, the default workspace is %q; want active — "+
			"single-tenant mode runs as this workspace and a suspended one answers 503", status)
	}

	if err := handle.SuspendDefaultWorkspace(ctx); err != nil {
		t.Fatalf("SuspendDefaultWorkspace: %v", err)
	}
	if err := handle.QueryRow(
		`SELECT status FROM workspaces WHERE id = ?`, db.DefaultWorkspaceID).Scan(&status); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if status != "suspended" {
		t.Errorf("after SuspendDefaultWorkspace the default workspace is %q; want suspended", status)
	}

	// Idempotent: boot happens more than once.
	if err := handle.SuspendDefaultWorkspace(ctx); err != nil {
		t.Errorf("second SuspendDefaultWorkspace: %v", err)
	}

	// The consequence that matters — it is no longer an active workspace, which is what
	// every sweep enumerates.
	var active int
	if err := handle.QueryRow(
		`SELECT COUNT(*) FROM workspaces WHERE status = 'active' AND id = ?`,
		db.DefaultWorkspaceID).Scan(&active); err != nil {
		t.Fatalf("count active default: %v", err)
	}
	if active != 0 {
		t.Errorf("the default workspace is still counted active; every background sweep " +
			"would keep taking a pass over it")
	}

	// A workspace that is not `default` is untouched: this is one row, not a policy.
	if _, err := handle.Exec(
		`INSERT INTO workspaces (id, slug, public_host, region, status) VALUES ('acme', 'acme', 'book.acme.test', 'us', 'active')`); err != nil {
		t.Fatalf("seed acme: %v", err)
	}
	if err := handle.SuspendDefaultWorkspace(ctx); err != nil {
		t.Fatalf("third SuspendDefaultWorkspace: %v", err)
	}
	if err := handle.QueryRow(`SELECT status FROM workspaces WHERE id = 'acme'`).Scan(&status); err != nil {
		t.Fatalf("read acme: %v", err)
	}
	if status != "active" {
		t.Errorf("acme is %q after suspending the default workspace; want active", status)
	}
}
