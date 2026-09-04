package db

import (
	"io/fs"
	"testing"
)

// TestMigrationDirs_parity is what lets TargetVersion be dialect-independent and
// what stops the two sets drifting: a migration added for one engine and
// forgotten for the other would otherwise only show up when someone ran the other
// engine.
func TestMigrationDirs_parity(t *testing.T) {
	sqliteFiles := migrationFiles(t, DialectSQLite)
	postgresFiles := migrationFiles(t, DialectPostgres)

	if len(sqliteFiles) != len(postgresFiles) {
		t.Fatalf("migration count differs: sqlite %d, postgres %d", len(sqliteFiles), len(postgresFiles))
	}

	for i, name := range sqliteFiles {
		if postgresFiles[i] != name {
			t.Errorf("migration %d differs: sqlite %q, postgres %q", i, name, postgresFiles[i])
		}
	}

	sqliteTarget, err := maxVersion(DialectSQLite.migrationsDir())
	if err != nil {
		t.Fatalf("maxVersion(sqlite): %v", err)
	}
	postgresTarget, err := maxVersion(DialectPostgres.migrationsDir())
	if err != nil {
		t.Fatalf("maxVersion(postgres): %v", err)
	}
	if sqliteTarget != postgresTarget {
		t.Errorf("target version differs: sqlite %d, postgres %d", sqliteTarget, postgresTarget)
	}
	if int(sqliteTarget) != len(sqliteFiles) {
		t.Errorf("target version %d does not match file count %d — a gap or a duplicate number",
			sqliteTarget, len(sqliteFiles))
	}
}

// migrationFiles lists a dialect's embedded migrations, sorted (fs.ReadDir sorts
// by name, which for NNNNN_ prefixes is version order).
func migrationFiles(t *testing.T, dialect Dialect) []string {
	t.Helper()

	entries, err := fs.ReadDir(migrations, dialect.migrationsDir())
	if err != nil {
		t.Fatalf("read %s: %v", dialect.migrationsDir(), err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
