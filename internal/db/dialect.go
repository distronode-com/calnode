package db

import (
	"database/sql"
	"strings"

	"github.com/jackc/pgx/v5/stdlib"
)

// Dialect names the SQL engine behind a handle.
//
// Calnode's SQL is hand-written and almost entirely portable. Two things are
// not: placeholder syntax (? versus $n), which the DB/Tx wrapper hides by
// rebinding every statement on its way through, and the handful of statements
// that use engine-specific functions, which callers resolve with Dialect.SQL.
type Dialect int

const (
	// DialectSQLite is the zero value deliberately: a handle whose dialect
	// could not be determined behaves exactly as it did before this package
	// knew about Postgres.
	DialectSQLite Dialect = iota
	DialectPostgres
)

// String returns the dialect's canonical lower-case name.
func (d Dialect) String() string {
	switch d {
	case DialectPostgres:
		return "postgres"
	default:
		return "sqlite"
	}
}

// SQL picks between two hand-written statements.
//
// Reach for this only when one portable statement is genuinely impossible —
// engine-specific functions (datetime('now'), strftime), upsert spelling, a
// PRAGMA. Differing placeholders are not a reason: the wrapper rebinds those,
// and duplicating a statement to change ? to $1 doubles the maintenance for no
// gain.
func (d Dialect) SQL(sqlite, postgres string) string {
	if d == DialectPostgres {
		return postgres
	}
	return sqlite
}

// Rebind converts a portable ?-placeholder statement into this dialect's form.
// SQLite takes ? natively, so its statements are returned untouched with no
// allocation.
func (d Dialect) Rebind(query string) string {
	if d != DialectPostgres {
		return query
	}
	return Rebind(query)
}

// driverName is the database/sql driver this dialect opens with.
func (d Dialect) driverName() string {
	if d == DialectPostgres {
		return "pgx"
	}
	return "sqlite"
}

// gooseDialect is goose's name for this engine.
func (d Dialect) gooseDialect() string {
	if d == DialectPostgres {
		return "postgres"
	}
	return "sqlite3"
}

// migrationsDir is the embedded directory holding this dialect's migrations.
// The two sets carry the same version numbers by construction — one schema, two
// spellings — which is what lets TargetVersion stay dialect-independent.
func (d Dialect) migrationsDir() string {
	if d == DialectPostgres {
		return "migrations/postgres"
	}
	return "migrations/sqlite"
}

// dialectFromURL classifies a DATABASE_URL. Only an explicit postgres URL
// selects Postgres, so every form that worked before still works unchanged:
// sqlite://./rel, sqlite:///abs, :memory:, and a bare file path.
func dialectFromURL(databaseURL string) Dialect {
	scheme := strings.ToLower(databaseURL)
	if strings.HasPrefix(scheme, "postgres://") || strings.HasPrefix(scheme, "postgresql://") {
		return DialectPostgres
	}
	return DialectSQLite
}

// dialectOf recovers the dialect from an already-open handle, for the
// package-level helpers that still take a bare *sql.DB. An unrecognised driver
// reads as SQLite: that is what every caller of those helpers was before, so an
// unknown driver degrades to the old behaviour rather than to an error.
func dialectOf(db *sql.DB) Dialect {
	if _, ok := db.Driver().(*stdlib.Driver); ok {
		return DialectPostgres
	}
	return DialectSQLite
}
