package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/config"
)

const (
	appDSN   = "postgres://calnode_app:pw@127.0.0.1:5432/calnode?sslmode=disable"
	adminDSN = "postgres://calnode_platform:pw@127.0.0.1:5432/calnode?sslmode=disable"
)

func TestLoad_multiTenantDefaultsOff(t *testing.T) {
	os.Unsetenv("MULTI_TENANT")
	os.Unsetenv("DATABASE_ADMIN_URL")
	os.Unsetenv("CALNODE_PLATFORM_TOKEN")

	cfg := config.Load()

	if cfg.MultiTenant {
		t.Error("MultiTenant should default to false")
	}
	if cfg.DatabaseAdminURL != "" {
		t.Errorf("DatabaseAdminURL = %q; want empty", cfg.DatabaseAdminURL)
	}
	if cfg.PlatformToken != "" {
		t.Errorf("PlatformToken = %q; want empty", cfg.PlatformToken)
	}
	// The default configuration is the one every existing deployment has, so it
	// has to validate.
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() on the default configuration: %v", err)
	}
}

func TestLoad_multiTenantEnv(t *testing.T) {
	t.Setenv("MULTI_TENANT", "1")
	t.Setenv("DATABASE_URL", appDSN)
	t.Setenv("DATABASE_ADMIN_URL", adminDSN)
	t.Setenv("CALNODE_PLATFORM_TOKEN", "tok")

	cfg := config.Load()

	if !cfg.MultiTenant {
		t.Error("MultiTenant = false; want true")
	}
	if cfg.DatabaseAdminURL != adminDSN {
		t.Errorf("DatabaseAdminURL = %q; want %q", cfg.DatabaseAdminURL, adminDSN)
	}
	if cfg.PlatformToken != "tok" {
		t.Errorf("PlatformToken = %q; want tok", cfg.PlatformToken)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() on a well-formed multi-tenant configuration: %v", err)
	}
}

// TestValidate_multiTenantRefusals covers every combination that cannot work.
// Each one is a refusal rather than a warning because its only other outcome is
// silent: a tenant reading another tenant's rows, or a demo reset wiping a fleet.
func TestValidate_multiTenantRefusals(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*config.Config)
		wantSub string
	}{
		{
			name:    "sqlite has no row-level security",
			mutate:  func(c *config.Config) { c.DatabaseURL = "sqlite://./data/calnode.db" },
			wantSub: "postgres:// DATABASE_URL",
		},
		{
			name:    "no platform DSN",
			mutate:  func(c *config.Config) { c.DatabaseAdminURL = "" },
			wantSub: "DATABASE_ADMIN_URL",
		},
		{
			name:    "platform DSN is not postgres",
			mutate:  func(c *config.Config) { c.DatabaseAdminURL = "sqlite://./data/calnode.db" },
			wantSub: "must be a postgres:// DSN",
		},
		{
			// The dangerous one: everything works, including reading other
			// tenants' rows, because a role that owns a table is not
			// constrained by that table's policy.
			name:    "one role for both handles",
			mutate:  func(c *config.Config) { c.DatabaseAdminURL = c.DatabaseURL },
			wantSub: "must differ from DATABASE_URL",
		},
		{
			name:    "demo mode wipes every tenant",
			mutate:  func(c *config.Config) { c.DemoMode = true },
			wantSub: "mutually exclusive",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				MultiTenant:      true,
				DatabaseURL:      appDSN,
				DatabaseAdminURL: adminDSN,
			}
			tc.mutate(cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil; want a refusal mentioning %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("Validate() = %q; want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

// TestValidate_singleTenantIgnoresTheRest is the byte-identical promise at the
// config layer: with MULTI_TENANT unset, none of the combinations above is an
// error, because none of the machinery they guard is running.
func TestValidate_singleTenantIgnoresTheRest(t *testing.T) {
	cfg := &config.Config{
		DatabaseURL:      "sqlite://./data/calnode.db",
		DatabaseAdminURL: "sqlite://./data/calnode.db",
		DemoMode:         true,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v; single-tenant mode should not police the multi-tenant knobs", err)
	}
}

func TestValidate_postgresqlSchemeAccepted(t *testing.T) {
	cfg := &config.Config{
		MultiTenant:      true,
		DatabaseURL:      "postgresql://app:pw@h/db",
		DatabaseAdminURL: "postgresql://platform:pw@h/db",
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v; postgresql:// is the same engine as postgres://", err)
	}
}
