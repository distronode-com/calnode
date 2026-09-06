package db

import (
	"context"
	"fmt"
)

// DefaultWorkspaceID is the tenant every row belongs to when MULTI_TENANT is
// unset. It is a literal rather than a generated id so that a single-tenant
// database migrated by 00060 reads the same on every machine, and so the SQLite
// column default can be a constant.
const DefaultWorkspaceID = "default"

// WorkspaceSetting is the PostgreSQL session parameter a tenant handle binds
// before every statement. The row-level-security policies compare
// workspace_id against it, and its two-argument current_setting form returns
// NULL when it has never been set — so an unbound session matches no row.
const WorkspaceSetting = "app.workspace_id"

// TenantTables lists every table that carries a workspace_id and a
// row-level-security policy. It is the Go copy of the list in the header of
// migration 00060, and TestTenancy_tableListsCoverTheSchema fails if either side
// drifts: a table added by a later migration must be classified here or in
// ExemptTables, so "nobody remembered to protect it" is not a possible outcome.
//
// Sorted, because the migration's ALTER and CREATE POLICY blocks are sorted and
// a reader diffing the two should not have to reorder one of them first.
var TenantTables = []string{
	"api_keys",
	"availability_overrides",
	"availability_rules",
	"booking_answers",
	"booking_attendees",
	"booking_hosts",
	"booking_manage_tokens",
	"bookings",
	"calendar_connections",
	"connection_calendars",
	"event_type_hosts",
	"event_type_questions",
	"event_type_reminders",
	"event_types",
	"idempotency_keys",
	"invite_tokens",
	"jobs",
	"magic_link_tokens",
	"meeting_consents",
	"notes",
	"oauth_access_tokens",
	"oauth_auth_codes",
	"recordings",
	"server_settings",
	"sessions",
	"team_members",
	"teams",
	"transcripts",
	"users",
	"webhook_deliveries",
	"webhooks",
	"zoom_connections",
}

// ExemptTables lists the tables that deliberately have no workspace_id.
//
//   - workspaces is the tenant root. It carries its own SELECT-only policy for
//     the application role instead: the host resolver, the suspended check and
//     publicURL all read it, and only the platform role writes it.
//   - crypto_keystore holds the wrapped DEK, and there is one DEK per process
//     (ARCHITECTURE §5). Per-tenant DEKs are a later hardening.
//   - goose_db_version is migration bookkeeping.
//   - oauth_clients is dynamic client registration, which is per client
//     APPLICATION rather than per tenant: one connector registration serves
//     every workspace it is later authorised against, and the grant that IS
//     per-tenant lives in oauth_access_tokens, which is a tenant table.
var ExemptTables = []string{
	"crypto_keystore",
	"goose_db_version",
	"oauth_clients",
	"workspaces",
}

// EnableRLS turns row-level security on, and forces it, for every tenant table.
// It is idempotent, so it can run on every boot, and it is a no-op on SQLite,
// which has no row-level security to enable.
//
// This is deliberately NOT part of migration 00060. FORCE ROW LEVEL SECURITY
// makes a table's policy apply to its OWNER as well, and in single-tenant mode
// DATABASE_URL is the owner: a schema migrated with FORCE and no
// app.workspace_id binding returns zero rows to its owner for every SELECT.
// Measured against PostgreSQL 17.11 with a NOBYPASSRLS owner role — and hidden
// entirely by a superuser DSN, since superusers bypass RLS, which is the shape
// of green that proves nothing. Gating the two ALTERs on MULTI_TENANT keeps the
// promise that a single-tenant database behaves exactly as it did before.
//
// The policies themselves live in the migration, where they are reviewable SQL.
// A policy on a table whose row-level security is not enabled is inert, so
// creating them unconditionally costs a single-tenant instance nothing.
//
// Table names are interpolated into the DDL because PostgreSQL takes no
// placeholder for an identifier. They come from TenantTables, a committed
// constant list, never from input.
func (h *DB) EnableRLS(ctx context.Context) error {
	if h.dialect != DialectPostgres {
		return nil
	}
	for _, table := range TenantTables {
		if _, err := h.DB.ExecContext(ctx, `ALTER TABLE `+table+` ENABLE ROW LEVEL SECURITY`); err != nil {
			return fmt.Errorf("enable row level security on %s: %w", table, err)
		}
		if _, err := h.DB.ExecContext(ctx, `ALTER TABLE `+table+` FORCE ROW LEVEL SECURITY`); err != nil {
			return fmt.Errorf("force row level security on %s: %w", table, err)
		}
	}
	return nil
}
