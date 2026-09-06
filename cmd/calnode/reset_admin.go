package main

import (
	"fmt"
	"os"

	"golang.org/x/crypto/bcrypt"

	"github.com/calnode/calnode/internal/config"
	"github.com/calnode/calnode/internal/db"
)

// runResetAdmin is invoked when the binary is called as:
//
//	calnode reset-admin [--workspace=<id>] <email> <new-password>
//
// It resets the password for the named user and enables email_login on their
// account. This is the last-resort recovery path when SMTP is unavailable and
// the admin is locked out.
func runResetAdmin(args []string) {
	cfg := config.Load()

	req, err := parseResetAdmin(args, cfg.MultiTenant)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	email, password := req.Email, req.Password

	// ⛔ In multi-tenant mode the UPDATE must be bound. users.email is unique per
	// WORKSPACE since D9, so `WHERE email = ?` on the platform handle would match
	// every workspace that has a user with that address and reset all of their
	// passwords — a recovery tool that hands out access to tenants the operator was
	// not asked about.
	var database *db.DB
	if cfg.MultiTenant {
		app, platform, oerr := db.OpenPair(cfg.DatabaseURL, cfg.DatabaseAdminURL)
		if oerr != nil {
			fmt.Fprintf(os.Stderr, "error: failed to open database pair: %v\n", oerr)
			os.Exit(1)
		}
		defer platform.Close()
		defer app.Close()
		database = app.ForWorkspace(req.Workspace)
		if database.Err() != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", database.Err())
			os.Exit(1)
		}
	} else {
		database, err = db.OpenDB(cfg.DatabaseURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to open database: %v\n", err)
			os.Exit(1)
		}
		defer database.Close()
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to hash password: %v\n", err)
		os.Exit(1)
	}

	res, err := database.Exec(
		`UPDATE users SET password_hash = ?, email_login = 1 WHERE email = ?`,
		string(hash), email)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: database update failed: %v\n", err)
		os.Exit(1)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		fmt.Fprintf(os.Stderr, "error: no user found with email %q\n", email)
		os.Exit(1)
	}

	fmt.Printf("password reset for %s — they can now log in with email + new password\n", email)
}
