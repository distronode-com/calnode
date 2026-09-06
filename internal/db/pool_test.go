package db_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/calnode/calnode/internal/config"
	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/dbtest"
)

// unreachablePostgresDSN is a syntactically valid Postgres URL pointed at
// nothing. sql.Open is lazy — pgx parses the DSN and no connection is attempted
// until a query runs — so the pool settings can be read back from Stats()
// without a server anywhere near the test.
const unreachablePostgresDSN = "postgres://calnode:pw@127.0.0.1:5432/calnode?sslmode=disable"

func TestOpenDB_poolSizeFromEnv(t *testing.T) {
	tests := []struct {
		name     string
		open     string // DB_MAX_OPEN_CONNS, "" = unset
		idle     string // DB_MAX_IDLE_CONNS, "" = unset
		wantOpen int
	}{
		{name: "unset uses the defaults", wantOpen: config.DefaultDBMaxOpenConns},
		{name: "raised", open: "40", idle: "10", wantOpen: 40},
		{name: "lowered to one", open: "1", idle: "1", wantOpen: 1},
		{name: "zero is not positive, so the default stands", open: "0", wantOpen: config.DefaultDBMaxOpenConns},
		{name: "negative is not positive either", open: "-4", wantOpen: config.DefaultDBMaxOpenConns},
		{name: "unparsable falls back", open: "lots", wantOpen: config.DefaultDBMaxOpenConns},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setPoolEnv(t, tc.open, tc.idle)

			handle, err := db.OpenDB(unreachablePostgresDSN)
			if err != nil {
				t.Fatalf("OpenDB: %v", err)
			}
			defer handle.Close()

			if got := handle.Stats().MaxOpenConnections; got != tc.wantOpen {
				t.Errorf("MaxOpenConnections = %d; want %d", got, tc.wantOpen)
			}
		})
	}
}

// TestOpenDB_withPoolBeatsEnv covers the escape hatch: a caller that must not
// follow the environment.
func TestOpenDB_withPoolBeatsEnv(t *testing.T) {
	setPoolEnv(t, "40", "20")

	handle, err := db.OpenDB(unreachablePostgresDSN, db.WithPool(3, 2))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer handle.Close()

	if got := handle.Stats().MaxOpenConnections; got != 3 {
		t.Errorf("MaxOpenConnections = %d; want 3 (WithPool, not the environment)", got)
	}
}

// TestOpenDB_sqlitePoolIsNotConfigurable is the correctness guarantee, not a
// preference: SQLite's single connection is what serialises write transactions
// (ARCHITECTURE §17, and it is why booking.lockHosts is a no-op there), and the
// pragmas are connection-scoped. An operator who sets DB_MAX_OPEN_CONNS for
// their Postgres instance and later moves the same environment onto a SQLite one
// must not silently lose that.
func TestOpenDB_sqlitePoolIsNotConfigurable(t *testing.T) {
	setPoolEnv(t, "40", "20")

	for _, url := range []string{
		"sqlite://:memory:",
		"sqlite://" + filepath.Join(t.TempDir(), "calnode.db"),
	} {
		handle, err := db.OpenDB(url, db.WithPool(40, 20))
		if err != nil {
			t.Fatalf("OpenDB(%s): %v", url, err)
		}
		if got := handle.Stats().MaxOpenConnections; got != 1 {
			t.Errorf("OpenDB(%s): MaxOpenConnections = %d; want 1 whatever the environment says", url, got)
		}
		handle.Close()
	}
}

// TestPostgres_idleLimitApplied measures the idle half against a real server.
// database/sql exposes the open limit through Stats() but not the idle one, so
// the only honest way to check SetMaxIdleConns took effect is to occupy several
// connections at once and count what the pool keeps when they are handed back.
func TestPostgres_idleLimitApplied(t *testing.T) {
	dsn := dbtest.PostgresDSN()
	if dsn == "" {
		t.Skipf("%s is not set; nothing to run against PostgreSQL", dbtest.DSNEnv)
	}

	const maxOpen, maxIdle = 4, 1
	handle, err := db.OpenDB(dsn, db.WithPool(maxOpen, maxIdle))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer handle.Close()

	ctx := context.Background()

	// A transaction holds a connection for its lifetime, so four of them force
	// four real connections open. No schema is touched: this is the pool, not the
	// database.
	txs := make([]*db.Tx, 0, maxOpen)
	for i := 0; i < maxOpen; i++ {
		tx, err := handle.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx %d: %v", i, err)
		}
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
			t.Fatalf("SELECT 1 in tx %d: %v", i, err)
		}
		txs = append(txs, tx)
	}
	if got := handle.Stats().OpenConnections; got != maxOpen {
		t.Errorf("OpenConnections while %d transactions are live = %d; want %d", maxOpen, got, maxOpen)
	}

	for i, tx := range txs {
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit %d: %v", i, err)
		}
	}

	// Every connection is back in the pool now; the idle limit decides how many
	// are kept rather than closed.
	if got := handle.Stats().Idle; got > maxIdle {
		t.Errorf("Idle after returning %d connections = %d; want at most %d", maxOpen, got, maxIdle)
	}
	t.Logf("pool: max open %d, max idle %d, idle after release %d",
		handle.Stats().MaxOpenConnections, maxIdle, handle.Stats().Idle)
}

// setPoolEnv sets or unsets both knobs for one test. t.Setenv restores the
// previous value at the end and refuses to run in a parallel test, which is what
// keeps these from leaking into the rest of the package.
func setPoolEnv(t *testing.T, open, idle string) {
	t.Helper()
	for _, kv := range []struct{ key, value string }{
		{"DB_MAX_OPEN_CONNS", open},
		{"DB_MAX_IDLE_CONNS", idle},
	} {
		if kv.value == "" {
			// t.Setenv has no "unset" mode; do it by hand and restore by hand.
			previous, had := os.LookupEnv(kv.key)
			os.Unsetenv(kv.key)
			t.Cleanup(func() {
				if had {
					os.Setenv(kv.key, previous)
				}
			})
			continue
		}
		t.Setenv(kv.key, kv.value)
	}
}
