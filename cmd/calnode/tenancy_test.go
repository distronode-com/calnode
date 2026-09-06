package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
)

// TestParseResetAdmin_requiresWorkspaceUnderMultiTenant.
//
// ⛔ The reason this is a refusal and not a default: since D9 the unique on users is
// (workspace_id, email), so an unscoped `UPDATE users SET password_hash = ? WHERE
// email = ?` matches every workspace that has a user with that address. A recovery
// tool that quietly resets three tenants' owners because they all use the same
// address is worse than one that refuses.
func TestParseResetAdmin_requiresWorkspaceUnderMultiTenant(t *testing.T) {
	_, err := parseResetAdmin([]string{"owner@example.com", "hunter2hunter2"}, true)
	if !errors.Is(err, errWorkspaceRequired) {
		t.Fatalf("err = %v; want errWorkspaceRequired", err)
	}
	// The message has to say what to do, not just that it refused.
	for _, want := range []string{"--workspace", "unique per", "every user"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %s", want, err)
		}
	}
}

func TestParseResetAdmin_acceptsBothFlagForms(t *testing.T) {
	for _, args := range [][]string{
		{"--workspace=acme", "owner@example.com", "hunter2hunter2"},
		{"--workspace", "acme", "owner@example.com", "hunter2hunter2"},
		{"owner@example.com", "hunter2hunter2", "--workspace=acme"},
	} {
		req, err := parseResetAdmin(args, true)
		if err != nil {
			t.Fatalf("parseResetAdmin(%v) = %v", args, err)
		}
		if req.Workspace != "acme" {
			t.Errorf("parseResetAdmin(%v) workspace = %q", args, req.Workspace)
		}
		if req.Email != "owner@example.com" || req.Password != "hunter2hunter2" {
			t.Errorf("parseResetAdmin(%v) = %+v", args, req)
		}
	}
}

// TestParseResetAdmin_singleTenantAcceptsAndIgnoresTheFlag: a script written for a
// multi-tenant fleet keeps working against a single-tenant instance.
func TestParseResetAdmin_singleTenantAcceptsAndIgnoresTheFlag(t *testing.T) {
	if _, err := parseResetAdmin([]string{"owner@example.com", "hunter2hunter2"}, false); err != nil {
		t.Errorf("single-tenant without --workspace: %v", err)
	}
	req, err := parseResetAdmin([]string{"--workspace=acme", "owner@example.com", "hunter2hunter2"}, false)
	if err != nil {
		t.Fatalf("single-tenant with --workspace: %v", err)
	}
	if req.Workspace != "acme" {
		t.Errorf("the flag was dropped: %+v", req)
	}
}

func TestParseResetAdmin_rejectsAMalformedWorkspace(t *testing.T) {
	for _, bad := range []string{"ACME", "ws a", "ws'a", "../etc", strings.Repeat("a", 65)} {
		if _, err := parseResetAdmin([]string{"--workspace=" + bad, "o@e.com", "hunter2hunter2"}, true); err == nil {
			t.Errorf("parseResetAdmin accepted --workspace=%q", bad)
		}
	}
}

func TestParseResetAdmin_stillChecksTheBasics(t *testing.T) {
	if _, err := parseResetAdmin([]string{"--workspace=acme", "o@e.com"}, true); err == nil {
		t.Error("accepted a missing password")
	}
	if _, err := parseResetAdmin([]string{"--workspace=acme", "o@e.com", "short"}, true); err == nil {
		t.Error("accepted a password under 8 characters")
	}
	if _, err := parseResetAdmin([]string{"--workspace"}, true); err == nil {
		t.Error("accepted --workspace with no value")
	}
}

// TestRefuseMCPStdio: the stdio transport has no credential and no Host, so there is
// nothing to resolve a tenant from.
func TestRefuseMCPStdio(t *testing.T) {
	if err := refuseMCPStdio(false); err != nil {
		t.Errorf("single-tenant stdio was refused: %v", err)
	}
	err := refuseMCPStdio(true)
	if !errors.Is(err, errMCPStdioMultiTenant) {
		t.Fatalf("err = %v; want errMCPStdioMultiTenant", err)
	}
	// It has to name the path that DOES work, or an operator is stuck.
	for _, want := range []string{"HTTP transport", "/mcp", "API key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %s", want, err)
		}
	}
}

// TestPlatformWideCLI pins the classification. rotate-key and recover-key touch only
// crypto_keystore, which 00060 leaves exempt (D2) because there is one DEK per
// process (D3) — so they are tenant-independent and take no --workspace.
func TestPlatformWideCLI(t *testing.T) {
	for _, cmd := range []string{"rotate-key", "recover-key"} {
		if !platformWideCLI[cmd] {
			t.Errorf("%s should be classified platform-wide", cmd)
		}
	}
	for _, cmd := range []string{"reset-admin", "mcp"} {
		if platformWideCLI[cmd] {
			t.Errorf("%s is not platform-wide: it touches tenant rows or needs a tenant", cmd)
		}
	}
}

// TestResetAdmin_scopesToTheNamedWorkspace is the behavioural half, and the reason
// the flag exists: two workspaces whose owners share an email address. The reset must
// change exactly one.
func TestResetAdmin_scopesToTheNamedWorkspace(t *testing.T) {
	app, platform := dbtest.RequireTenantPair(t)
	ctx := context.Background()

	const shared = "owner@example.com"
	for _, ws := range []string{"acme", "globex"} {
		if _, err := platform.ExecContext(ctx,
			`INSERT INTO workspaces (id, slug, public_host, region, status) VALUES (?, ?, ?, '', 'active')`,
			ws, ws, ws+".example.com"); err != nil {
			t.Fatalf("workspace %s: %v", ws, err)
		}
		// ⛔ The same address in both. Legal since D9 made the unique
		// (workspace_id, email), and it is the ordinary case for an agency running
		// several client workspaces.
		if _, err := app.ForWorkspace(ws).ExecContext(ctx,
			`INSERT INTO users (id, email, name, password_hash, email_login) VALUES (?, ?, ?, 'old', 0)`,
			ws+"-owner", shared, ws+" owner"); err != nil {
			t.Fatalf("user %s: %v", ws, err)
		}
	}

	req, err := parseResetAdmin([]string{"--workspace=acme", shared, "hunter2hunter2"}, true)
	if err != nil {
		t.Fatalf("parseResetAdmin: %v", err)
	}

	// The same statement runResetAdmin runs, on the same bound handle it builds.
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	res, err := app.ForWorkspace(req.Workspace).ExecContext(ctx,
		`UPDATE users SET password_hash = ?, email_login = 1 WHERE email = ?`, string(hash), req.Email)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		t.Errorf("the reset touched %d rows; want exactly 1", n)
	}

	// acme's owner can log in with the new password; globex's is untouched.
	for _, tc := range []struct {
		ws          string
		wantChanged bool
	}{{"acme", true}, {"globex", false}} {
		var stored string
		var login int
		if err := platform.QueryRowContext(ctx,
			`SELECT password_hash, email_login FROM users WHERE workspace_id = ?`, tc.ws).
			Scan(&stored, &login); err != nil {
			t.Fatalf("read %s's owner: %v", tc.ws, err)
		}
		changed := stored != "old"
		if changed != tc.wantChanged {
			t.Errorf("%s's password changed = %v; want %v", tc.ws, changed, tc.wantChanged)
		}
		if tc.wantChanged && login != 1 {
			t.Errorf("%s's email_login was not enabled", tc.ws)
		}
		if !tc.wantChanged && login != 0 {
			t.Errorf("%s's email_login was enabled by another workspace's reset", tc.ws)
		}
	}
}

// TestResetAdmin_theOldShapeResetsEveryWorkspace is the negative control: the
// unscoped statement, on the platform handle, which is what the command did before
// --workspace existed.
func TestResetAdmin_theOldShapeResetsEveryWorkspace(t *testing.T) {
	app, platform := dbtest.RequireTenantPair(t)
	ctx := context.Background()

	const shared = "owner@example.com"
	for _, ws := range []string{"acme", "globex"} {
		if _, err := platform.ExecContext(ctx,
			`INSERT INTO workspaces (id, slug, public_host, region, status) VALUES (?, ?, ?, '', 'active')`,
			ws, ws, ws+".example.com"); err != nil {
			t.Fatalf("workspace %s: %v", ws, err)
		}
		if _, err := app.ForWorkspace(ws).ExecContext(ctx,
			`INSERT INTO users (id, email, name, password_hash) VALUES (?, ?, ?, 'old')`,
			ws+"-owner", shared, ws+" owner"); err != nil {
			t.Fatalf("user %s: %v", ws, err)
		}
	}

	res, err := platform.ExecContext(ctx,
		`UPDATE users SET password_hash = 'new', email_login = 1 WHERE email = ?`, shared)
	if err != nil {
		t.Fatalf("unscoped reset: %v", err)
	}
	n, _ := res.RowsAffected()
	if n != 2 {
		t.Fatalf("the unscoped reset touched %d rows; the control proves nothing if it is not 2", n)
	}
	t.Logf("negative control: one unscoped `WHERE email = %q` reset %d workspaces' owners — "+
		"since D9 the unique is (workspace_id, email), so a shared address is legal and common", shared, n)

	_ = db.DefaultWorkspaceID // the default workspace has no user with this address
}
