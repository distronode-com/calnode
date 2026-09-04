package db

import (
	"database/sql"
	"testing"
)

func TestDialectFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want Dialect
	}{
		{"sqlite://./data/calnode.db", DialectSQLite},
		{"sqlite:///var/lib/calnode/calnode.db", DialectSQLite},
		{"sqlite://:memory:", DialectSQLite},
		{"sqlite://file::memory:?cache=shared&_fk=1", DialectSQLite},
		{"./data/calnode.db", DialectSQLite},
		{"/var/lib/calnode/calnode.db", DialectSQLite},
		{":memory:", DialectSQLite},
		{"", DialectSQLite},
		{"postgres://calnode@localhost:5432/calnode", DialectPostgres},
		{"postgresql://calnode@localhost:5432/calnode", DialectPostgres},
		{"postgres://u:p@h:5432/d?sslmode=require", DialectPostgres},
		{"POSTGRES://u:p@h:5432/d", DialectPostgres},
		// A path that merely mentions postgres is still a SQLite file.
		{"./postgres-backup/calnode.db", DialectSQLite},
	}

	for _, tt := range tests {
		if got := dialectFromURL(tt.url); got != tt.want {
			t.Errorf("dialectFromURL(%q) = %v; want %v", tt.url, got, tt.want)
		}
	}
}

func TestParseDSN(t *testing.T) {
	tests := []struct{ url, want string }{
		{"sqlite://./data/calnode.db", "./data/calnode.db"},
		{"sqlite:///var/lib/calnode/calnode.db", "/var/lib/calnode/calnode.db"},
		{"sqlite://:memory:", ":memory:"},
		{"sqlite://file::memory:?cache=shared&_fk=1", "file::memory:?cache=shared&_fk=1"},
		{"./data/calnode.db", "./data/calnode.db"},
		// Windows: sqlite:///C:/path/db → C:/path/db.
		{"sqlite:///C:/calnode/calnode.db", "C:/calnode/calnode.db"},
	}

	for _, tt := range tests {
		if got := parseDSN(tt.url); got != tt.want {
			t.Errorf("parseDSN(%q) = %q; want %q", tt.url, got, tt.want)
		}
	}
}

// TestDialectOf covers the driver sniffing the package-level Migrate and the
// version helpers rely on. It needs no server: database/sql is lazy, so both
// handles exist without a connection.
func TestDialectOf(t *testing.T) {
	sqlite, err := sql.Open(DialectSQLite.driverName(), ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer sqlite.Close()

	postgres, err := sql.Open(DialectPostgres.driverName(), "postgres://u:p@127.0.0.1:5432/d")
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer postgres.Close()

	if got := dialectOf(sqlite); got != DialectSQLite {
		t.Errorf("dialectOf(sqlite handle) = %v; want %v", got, DialectSQLite)
	}
	if got := dialectOf(postgres); got != DialectPostgres {
		t.Errorf("dialectOf(postgres handle) = %v; want %v", got, DialectPostgres)
	}
}

func TestDialectNames(t *testing.T) {
	tests := []struct {
		dialect        Dialect
		driver, goose  string
		migrationsPath string
	}{
		{DialectSQLite, "sqlite", "sqlite3", "migrations/sqlite"},
		{DialectPostgres, "pgx", "postgres", "migrations/postgres"},
	}

	for _, tt := range tests {
		if got := tt.dialect.driverName(); got != tt.driver {
			t.Errorf("%v.driverName() = %q; want %q", tt.dialect, got, tt.driver)
		}
		if got := tt.dialect.gooseDialect(); got != tt.goose {
			t.Errorf("%v.gooseDialect() = %q; want %q", tt.dialect, got, tt.goose)
		}
		if got := tt.dialect.migrationsDir(); got != tt.migrationsPath {
			t.Errorf("%v.migrationsDir() = %q; want %q", tt.dialect, got, tt.migrationsPath)
		}
	}
}
