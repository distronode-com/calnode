package main

import (
	"errors"
	"fmt"
	"strings"
)

// The CLI subcommands under multi-tenant mode.
//
// Each of the four is one of two things, and which it is follows from what it
// touches:
//
//	rotate-key, recover-key  — crypto_keystore, which is EXEMPT from tenancy (D2)
//	                           and holds one DEK per process (D3). Platform-wide by
//	                           construction, so they run unchanged on the platform
//	                           handle and need no workspace.
//	reset-admin              — users, a tenant table. ⛔ And since D9 made the
//	                           unique (workspace_id, email) rather than email, an
//	                           unscoped UPDATE ... WHERE email = ? matches EVERY
//	                           workspace that has a user with that address and
//	                           resets all of their passwords. It needs --workspace.
//	mcp                      — the stdio transport has no credential and no Host, so
//	                           there is nothing to resolve a tenant from. Refused.
var (
	// errWorkspaceRequired is reset-admin's refusal under MULTI_TENANT.
	errWorkspaceRequired = errors.New(
		"reset-admin needs --workspace=<id> when MULTI_TENANT is set: users.email is unique per " +
			"workspace, so an unscoped reset would change the password of every user with that " +
			"address in every workspace")

	// errMCPStdioMultiTenant is the stdio transport's refusal.
	errMCPStdioMultiTenant = errors.New(
		"calnode mcp (stdio) is not available when MULTI_TENANT is set: the stdio transport carries " +
			"no credential and no Host, so there is no workspace to resolve and the tools would run " +
			"unscoped. Use the HTTP transport at POST /mcp with a workspace's API key or OAuth token, " +
			"which resolves the tenant from the credential")
)

// resetAdminRequest is a parsed `calnode reset-admin` invocation.
type resetAdminRequest struct {
	Email     string
	Password  string
	Workspace string // "" in single-tenant mode
}

// parseResetAdmin parses the subcommand's arguments and applies the multi-tenant
// rule. --workspace may appear anywhere among the positional arguments, in either
// the `--workspace=id` or `--workspace id` form.
//
// In single-tenant mode --workspace is accepted and ignored rather than rejected,
// so a script written for a multi-tenant fleet keeps working against a
// single-tenant instance.
func parseResetAdmin(args []string, multiTenant bool) (resetAdminRequest, error) {
	var req resetAdminRequest
	var positional []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--workspace="):
			req.Workspace = strings.TrimPrefix(a, "--workspace=")
		case a == "--workspace":
			if i+1 >= len(args) {
				return req, errors.New("--workspace needs a value")
			}
			i++
			req.Workspace = args[i]
		default:
			positional = append(positional, a)
		}
	}

	if len(positional) != 2 {
		return req, errors.New("usage: calnode reset-admin [--workspace=<id>] <email> <new-password>")
	}
	req.Email = strings.TrimSpace(strings.ToLower(positional[0]))
	req.Password = positional[1]

	if len(req.Password) < 8 || len(req.Password) > 72 {
		return req, errors.New("password must be 8–72 characters")
	}

	if multiTenant {
		if req.Workspace == "" {
			return req, errWorkspaceRequired
		}
		if !validWorkspaceID(req.Workspace) {
			return req, fmt.Errorf("--workspace=%q is not a workspace id ([a-z0-9_-], 1-64 chars)", req.Workspace)
		}
	}

	return req, nil
}

// refuseMCPStdio reports whether `calnode mcp` may run, and why not.
func refuseMCPStdio(multiTenant bool) error {
	if multiTenant {
		return errMCPStdioMultiTenant
	}
	return nil
}

// platformWideCLI names the subcommands that operate on crypto_keystore and are
// therefore tenant-independent. Kept as data so the test can assert the
// classification rather than re-derive it.
var platformWideCLI = map[string]bool{
	"rotate-key":  true,
	"recover-key": true,
}

// validWorkspaceID mirrors db.ValidWorkspaceID without importing it, because this
// runs before any database handle exists.
func validWorkspaceID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
