// Package dbtest opens a migrated database for a test, on whichever engine the
// environment selects.
//
// Unset CALNODE_TEST_POSTGRES_DSN — the default, and what upstream CI and every
// other contributor sees — means in-memory SQLite, exactly as before. Set it, and
// the same tests run against that PostgreSQL server, each in a schema of its own
// that is created before the migrations and dropped afterwards. Nothing here changes
// what a test asserts; it changes which engine has to satisfy it.
package dbtest

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/calnode/calnode/internal/db"
)

// DSNEnv names the environment variable that switches the suite onto PostgreSQL.
const DSNEnv = "CALNODE_TEST_POSTGRES_DSN"

// PostgresDSN returns the configured PostgreSQL DSN, or "" when the suite should
// run on SQLite.
func PostgresDSN() string { return os.Getenv(DSNEnv) }

// Open returns a migrated handle for t, on Postgres when DSNEnv is set and on
// in-memory SQLite otherwise. The handle is closed when t finishes.
func Open(t *testing.T) *db.DB {
	t.Helper()
	if dsn := PostgresDSN(); dsn != "" {
		return openPostgres(t, dsn)
	}
	return openSQLite(t)
}

// RequirePostgres returns a migrated Postgres handle, skipping t when DSNEnv is
// unset. For the tests that are only meaningful on Postgres — a race the SQLite
// pool makes impossible, say.
func RequirePostgres(t *testing.T) *db.DB {
	t.Helper()
	dsn := PostgresDSN()
	if dsn == "" {
		t.Skipf("%s is not set; nothing to run against PostgreSQL", DSNEnv)
	}
	return openPostgres(t, dsn)
}

func openSQLite(t *testing.T) *db.DB {
	t.Helper()
	h, err := db.OpenDB("sqlite://:memory:")
	if err != nil {
		t.Fatalf("dbtest: open sqlite: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	if err := h.Migrate(); err != nil {
		t.Fatalf("dbtest: migrate sqlite: %v", err)
	}
	return h
}

// openPostgres gives t its own schema on the shared server.
//
// A schema rather than a database: CREATE DATABASE cannot run inside a
// transaction, takes seconds on a busy server, and needs rights a CI service
// container may not grant the test role. A schema is one statement, isolates
// tables and goose's own bookkeeping equally well, and disappears whole with
// DROP ... CASCADE. Everything reaches it through search_path on the connection,
// so no query in the tree needs to name it.
func openPostgres(t *testing.T, dsn string) *db.DB {
	t.Helper()

	schema := "calnode_test_" + randomSuffix(t)

	// A second handle on the default search_path, kept open only to create and drop
	// the schema: the drop cannot run through a connection whose search_path points
	// at the schema being dropped.
	admin, err := db.OpenDB(dsn)
	if err != nil {
		t.Fatalf("dbtest: open postgres (admin): %v", err)
	}
	if _, err := admin.Exec(`CREATE SCHEMA ` + quoteIdent(schema)); err != nil {
		admin.Close()
		t.Fatalf("dbtest: create schema %s: %v", schema, err)
	}
	// Registered first, so it runs last: the schema is dropped after the handle
	// below has been closed.
	t.Cleanup(func() {
		defer admin.Close()
		if _, err := admin.Exec(`DROP SCHEMA ` + quoteIdent(schema) + ` CASCADE`); err != nil {
			t.Errorf("dbtest: drop schema %s: %v", schema, err)
		}
	})

	h, err := db.OpenDB(withSearchPath(t, dsn, schema))
	if err != nil {
		t.Fatalf("dbtest: open postgres: %v", err)
	}
	t.Cleanup(func() { h.Close() })

	if err := h.Migrate(); err != nil {
		t.Fatalf("dbtest: migrate postgres: %v", err)
	}
	return h
}

// withSearchPath returns dsn with search_path pointed at schema. pgx passes
// unrecognised query parameters through as PostgreSQL runtime parameters, so this
// is the whole of the isolation: goose writes goose_db_version there, the
// migrations create their tables there, and every unqualified name in the tree
// resolves there.
func withSearchPath(t *testing.T, dsn, schema string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("dbtest: parse %s: %v", DSNEnv, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// randomSuffix keeps concurrent runs — two packages under `go test ./...`, or two
// CI jobs against one server — out of each other's schemas.
func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("dbtest: random schema name: %v", err)
	}
	return hex.EncodeToString(b)
}

// quoteIdent quotes a generated schema name. The names are hex from the line
// above, so this is belt and braces rather than a defence — but a schema name
// interpolated into DDL unquoted is a bad habit to leave lying around for the next
// person who passes something else in.
func quoteIdent(name string) string {
	return `"` + name + `"`
}
