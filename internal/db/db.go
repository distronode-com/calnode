package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/sqlite/*.sql migrations/postgres/*.sql
var migrations embed.FS

// OpenDB connects to the database named by databaseURL and configures the pool
// for the engine it names.
//
// There is deliberately no bare-handle sibling. An Open returning *sql.DB
// existed through the port "for callers that have not moved over yet", and it
// was a foot-gun with no upside: statements issued through it are not rebound,
// so every ? in them is a syntax error on Postgres, found at runtime and far
// from the call. Anything that genuinely needs the bare pool (goose, Litestream)
// reaches it as handle.DB, which at least says so at the call site.
//
// URL formats:
//
//	sqlite://./path/to/db, sqlite:///absolute/path, or a bare file path
//	postgres://user:pass@host:port/dbname (postgresql:// is accepted too)
func OpenDB(databaseURL string) (*DB, error) {
	if dialectFromURL(databaseURL) == DialectPostgres {
		return openPostgres(databaseURL)
	}
	return openSQLite(databaseURL)
}

// openSQLite opens SQLite and configures pragmas.
func openSQLite(databaseURL string) (*DB, error) {
	dsn := parseDSN(databaseURL)

	db, err := sql.Open(DialectSQLite.driverName(), dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// SQLite performs best with a single writer connection; WAL allows
	// concurrent readers. Keeping max open conns at 1 prevents "database is
	// locked" under concurrent writes without WAL tuning.
	// SetConnMaxLifetime(0) and SetMaxIdleConns(1) ensure the single connection
	// is never recycled — PRAGMAs are connection-scoped and must not be lost.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}

	return &DB{DB: db, dialect: DialectSQLite}, nil
}

// openPostgres opens a PostgreSQL pool.
//
// The one-connection pool above is a SQLite constraint, not a Calnode design
// choice, and carrying it over would serialise the whole instance on a database
// that has its own concurrency control. Sizes are deliberately modest: one
// Calnode instance is one small process, and a self-hoster's Postgres is usually
// sized to match.
//
// The pool does cost one property the SQLite path gets by accident: the
// booking-overlap check (ARCHITECTURE §17) is free of TOCTOU races there only
// because every transaction queues on that single connection. Here two
// overlapping bookings can clear the check concurrently, and only
// idx_bookings_no_double — exact start times — stops them. Closing that gap
// belongs with the booking transaction (SERIALIZABLE, or a range exclusion
// constraint), not with the pool.
func openPostgres(databaseURL string) (*DB, error) {
	// pgx parses the DSN here, so a malformed URL fails at Open. Reachability is
	// not probed: Migrate runs immediately after Open in every entry point and
	// reports an unreachable server with the same context a probe would.
	db, err := sql.Open(DialectPostgres.driverName(), databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)

	return &DB{DB: db, dialect: DialectPostgres}, nil
}

// Migrate runs any pending Goose migrations embedded for this handle's dialect.
func (h *DB) Migrate() error {
	return migrate(h.DB, h.dialect)
}

// Migrate runs any pending Goose migrations embedded for db's engine, which is
// recovered from its driver.
func Migrate(db *sql.DB) error {
	return migrate(db, dialectOf(db))
}

// gooseMu guards goose's package-level dialect and base FS. A running Calnode
// only ever uses one engine, but the tests migrate both in one process and the
// two settings must not interleave.
var gooseMu sync.Mutex

func migrate(db *sql.DB, dialect Dialect) error {
	gooseMu.Lock()
	defer gooseMu.Unlock()

	goose.SetBaseFS(migrations)

	if err := goose.SetDialect(dialect.gooseDialect()); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}

	if err := goose.Up(db, dialect.migrationsDir()); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	return nil
}

var (
	targetVersionOnce sync.Once
	targetVersion     int64
	targetVersionErr  error
)

// TargetVersion returns the highest migration version embedded in the binary —
// i.e. the schema version a fully-migrated database should report.
//
// It is dialect-independent: the per-dialect directories are two spellings of
// one schema and carry the same version numbers, which TestMigrationDirs_parity
// enforces.
func TargetVersion() (int64, error) {
	targetVersionOnce.Do(func() {
		targetVersion, targetVersionErr = maxVersion(DialectSQLite.migrationsDir())
	})
	return targetVersion, targetVersionErr
}

// maxVersion returns the highest goose version number in an embedded migrations
// directory.
func maxVersion(dir string) (int64, error) {
	entries, err := fs.ReadDir(migrations, dir)
	if err != nil {
		return 0, fmt.Errorf("read embedded migrations: %w", err)
	}
	var highest int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		// Filenames are "NNNNN_description.sql"; the leading number is the version.
		name := path.Base(e.Name())
		numPart, _, _ := strings.Cut(name, "_")
		v, err := strconv.ParseInt(numPart, 10, 64)
		if err != nil {
			continue // ignore files that don't follow the goose naming convention
		}
		if v > highest {
			highest = v
		}
	}
	return highest, nil
}

// AppliedVersion returns the schema version currently applied to db by reading
// goose's bookkeeping table directly (no goose global state). A missing
// goose_db_version table returns an error, which callers treat as "not migrated".
//
// is_applied is tested for truth rather than compared to 1: goose stores it as an
// INTEGER on SQLite and a BOOLEAN on Postgres, and the bare column is the one
// spelling both engines accept.
func AppliedVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var v sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied`).Scan(&v)
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Int64, nil
}

// SchemaReady reports whether db has been migrated to the embedded target
// version. The provisioner / load balancer can poll /readyz, which calls this,
// to gate traffic until migrations have finished.
func SchemaReady(ctx context.Context, db *sql.DB) (bool, error) {
	target, err := TargetVersion()
	if err != nil {
		return false, err
	}
	applied, err := AppliedVersion(ctx, db)
	if err != nil {
		return false, err
	}
	return applied >= target, nil
}

// SchemaReady is the handle-level spelling, for callers that hold a *DB — which
// is every caller in the tree. The package-level functions above stay for the
// bare-pool cases (goose's own bookkeeping, the tests that open an unmigrated
// pool), but a handler reaching into h.db.DB to answer a readiness probe was one
// more place where the exported embedded field looked like the normal way to do
// things.
func (h *DB) SchemaReady(ctx context.Context) (bool, error) {
	return SchemaReady(ctx, h.DB)
}

// AppliedVersion is the handle-level spelling of the package function.
func (h *DB) AppliedVersion(ctx context.Context) (int64, error) {
	return AppliedVersion(ctx, h.DB)
}

func parseDSN(url string) string {
	// Strip scheme prefix: sqlite:// → remainder
	dsn := strings.TrimPrefix(url, "sqlite://")
	// sqlite:///absolute/path → /absolute/path (triple slash, first two stripped above)
	// sqlite://./relative     → ./relative
	// On Windows, sqlite:///C:/path/db → /C:/path/db — strip the leading slash so
	// the result is a valid Windows absolute path (C:/path/db).
	if len(dsn) >= 3 && dsn[0] == '/' && dsn[2] == ':' {
		dsn = dsn[1:]
	}
	return dsn
}
