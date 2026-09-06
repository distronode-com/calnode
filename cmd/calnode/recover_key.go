package main

import (
	"fmt"
	"os"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/keyvault"
)

// runRecoverKey is invoked as: calnode recover-key <new-platform-secret>
//
// Use this when the platform secret (CALNODE_ENCRYPTION_KEY) has been lost.
// It unwraps the DEK via the recovery keystore entry (which was wrapped under
// CALNODE_RECOVERY_SECRET at first boot) and re-establishes a new primary.
//
// CALNODE_RECOVERY_SECRET must be in the environment.
// After a successful recovery, set CALNODE_ENCRYPTION_KEY=<new-platform-secret>
// before the next server start.
// Multi-tenant mode needs no change here, and this note is why rather than an
// oversight: recover-key operates only on crypto_keystore, which migration 00060 leaves
// EXEMPT from tenancy (D2) because there is one DEK per process (D3). It is
// platform-wide by construction, so DATABASE_URL's handle reaches it whether or not
// the row-level-security policies are enabled — crypto_keystore has none.
//
// ⚠️ If per-tenant DEKs are ever adopted, both commands need a --workspace flag and
// this comment becomes wrong. cmd/calnode/tenancy.go's platformWideCLI is the list a
// test asserts against.
func runRecoverKey(args []string) {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "usage: calnode recover-key <new-platform-secret>")
		fmt.Fprintln(os.Stderr, "       CALNODE_RECOVERY_SECRET must be set in the environment")
		os.Exit(1)
	}
	newSecret := args[0]

	recoverySecret := os.Getenv("CALNODE_RECOVERY_SECRET")
	if recoverySecret == "" {
		fmt.Fprintln(os.Stderr, "error: CALNODE_RECOVERY_SECRET is not set")
		os.Exit(1)
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "sqlite://./data/calnode.db"
	}

	database, err := db.OpenDB(dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open database: %v\n", err)
		os.Exit(1)
	}
	defer database.Close()

	if err := keyvault.RecoverPrimary(database, recoverySecret, newSecret); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("recovery complete — set CALNODE_ENCRYPTION_KEY to the new secret before the next server start")
}
