# PROGRESS — Postgres call sites (`feat/postgres-callsites`)

Converting Calnode's call sites onto the dialect-aware `*db.DB` / `*db.Tx` wrapper
(`internal/db`, built on the branch's base commit) so the app runs on PostgreSQL as
well as SQLite. SQLite behaviour must not change.

## Boundary 1 — thread the type through — DONE

Every struct field, constructor, package-level helper and test helper that held
`*sql.DB` now holds `*db.DB`; `*sql.Tx` became `*db.Tx`. Call sites needed no edit:
the wrapper's method names match `database/sql`'s.

- `db.Open` → `db.OpenDB` and `db.Migrate(x)` → `x.Migrate()` in `cmd/calnode/*` and
  every test helper.
- `internal/connstore`: `Execer` is an interface, so it needed no change; its doc
  comment now names `*db.DB` / `*db.Tx`. `destination_test.go` deliberately opens a
  bare `sql.Open("sqlite", ":memory:")` against a bespoke fragment schema and stays
  `*sql.DB` — `Execer` accepts it.
- `internal/handler/health.go` passes `h.db.DB` to `db.SchemaReady`, which takes a
  bare `*sql.DB`. Legitimate: its one statement carries no placeholders.

Gates: `go build ./...`, `go vet ./...`, `gofmt -l`, `go test ./...` all clean on SQLite.

## Boundary 2 — non-portable SQL — DONE

New package `internal/dbtime`: `Now()` for `datetime('now')`, `NowMilli()` for
`strftime('%Y-%m-%dT%H:%M:%fZ','now')`. Every one of the 38 engine-side timestamps
became a bound parameter. The two layouts are kept distinct on purpose — the schema
already stores both shapes, they are compared lexicographically and served to
clients verbatim, so folding them into one would change SQLite's stored bytes.
`dbtime_test.go` asserts both layouts against SQLite's own output.

- `INSERT OR IGNORE` → `INSERT … ON CONFLICT DO NOTHING`, no conflict target, which
  both engines accept and which matches OR IGNORE's "any unique constraint" scope
  (3 sites: demo seed, and the two reminder enqueues).
- `COLLATE NOCASE` → `LOWER(col) = LOWER(?)` at both sites. No `d.SQL` pair: nothing
  indexes `booking_attendees.email`, so there is no collation for an index to depend
  on and no plan to preserve.
- `PRAGMA foreign_keys` in `demo.Reset` is now SQLite-only. Postgres has no
  equivalent short of superuser rights, so that branch wipes with one
  `TRUNCATE … CASCADE` naming every table, which needs no FK ordering.
- `sqlite_master` in `demo.listTables` is the one `d.SQL` pair: `pg_tables` scoped to
  `current_schema()` on Postgres, which also keeps it correct inside an isolated
  test schema.
- `RETURNING position` (`question_handler.go`) is unchanged. Both engines support it;
  verified on SQLite by the existing question tests, unverified on Postgres.
- No `LIMIT` inside any `UPDATE`/`DELETE`. Checked by scanning every backquoted
  string in the tree, not by a line grep.

Gates: `go build ./...`, `go vet ./...`, `gofmt -l`, `go test ./...` clean on SQLite.

## Boundary 3 — advisory lock replacing the single-writer guarantee — DONE

`internal/booking/hostlock.go`. `lockHosts` takes `pg_advisory_xact_lock` on each
host whose availability the transaction is about to decide, at the start of the
transaction and before the first `hostBusy` read. On SQLite it returns immediately
and the `SetMaxOpenConns(1)` guarantee stands untouched.

- **Key derivation**: `int64(binary.BigEndian.Uint64(sha256.Sum256("calnode:booking:host:" + hostID)[:8]))`.
  Domain-separated so a future advisory lock on another entity cannot collide by
  hashing the same raw id. A collision between two hosts would cost an unnecessary
  serialisation, never a wrong answer, so stability and spread are what matter.
  `hostlock_internal_test.go` pins two values computed independently of the code.
- **Deadlock**: ids are sorted and deduplicated on a copy before locking, so two
  transactions needing the same pair cannot take them in opposite orders. The copy
  matters because `Create`'s `HostIDs` arrive in round-robin priority order.
- **Three call sites, not two.** `Create` and `Reschedule` as specified, plus
  `ReassignHost`, which has the identical check-then-write shape (`hostBusy`'s own
  doc names all three). Leaving it out would have left a known hole.
- `Reschedule` locks the primary host *before* reading `booking_hosts`, not after: a
  concurrent `ReassignHost` holds that same key, and that ordering is what stops a
  reassignment committing between the host-list read and the `UPDATE`.
- `isUniqueViolation` now recognises Postgres by SQLSTATE 23505 (`pgconn.PgError`)
  as well as SQLite by message. Without it the index's backstop returned a 500
  instead of `ErrDoubleBooked` on Postgres.
- The materialise-into-a-slice patterns in `Reschedule` and the calendar reconciler
  are untouched; they are still required on SQLite.

**Measured, against the live PostgreSQL 17 on 127.0.0.1:55432**, with a temporary
hand-written minimal schema because `migrations/postgres` does not exist yet. Two
goroutines race overlapping-but-not-identical slots (10:00–10:30 against
10:15–10:45, so `idx_bookings_no_double` cannot catch it), 40 rounds:

| | created | conflicts | overlapping pairs in the DB |
|---|---|---|---|
| lock in place | 40 | 40 | **0** |
| lock disabled | 79 | 1 | **39** |

The negative control is the point: unlocked, the partial unique index caught 1 of
40. The committed test (`internal/booking/concurrency_test.go`) is the same race
through `dbtest.RequirePostgres`, and skips on SQLite saying the single-connection
pool makes the race impossible.

## Boundary 4 — Postgres test harness, docs, CI — IN PROGRESS

`internal/dbtest` landed with Boundary 3 because the concurrency test needs it.
`Open(t)` returns in-memory SQLite unless `CALNODE_TEST_POSTGRES_DSN` is set, in
which case each test gets its own `calnode_test_<random>` schema, created before
the migrations and dropped after. Isolation rides on `search_path` in the DSN,
which `dbtest_test.go` verifies against a live server rather than assuming
(`TestSearchPathIsolation` passes; it also asserts the schema is invisible from the
default path).

## Blocked on the database layer

`internal/db/migrations/postgres` does not exist yet (`db.go` embeds
`migrations/sqlite/*.sql` only), so nothing can migrate on Postgres and no
Postgres-side run can be verified from this branch yet. The harness added in
Boundary 4 calls `h.Migrate()` and starts working the moment those land.
