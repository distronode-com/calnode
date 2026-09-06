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

## Boundary 4 — Postgres test harness, docs, CI — DONE

`internal/dbtest` landed with Boundary 3 because the concurrency test needs it.
`Open(t)` returns in-memory SQLite unless `CALNODE_TEST_POSTGRES_DSN` is set, in
which case each test gets its own `calnode_test_<random>` schema, created before
the migrations and dropped after. Isolation rides on `search_path` in the DSN,
which `dbtest_test.go` verifies against a live server rather than assuming
(`TestSearchPathIsolation` passes; it also asserts the schema is invisible from the
default path).

- **22 test helpers across 18 files** repointed from `db.OpenDB("sqlite://:memory:")`
  + `Migrate()` onto `dbtest.Open(t)`, so the whole suite follows the environment.
  Deliberately left on SQLite: `dbtime_test.go` (it pins SQLite's own output),
  `hostlock_internal_test.go` (it asserts the lock is a no-op there),
  `connstore/destination_test.go` (bare driver, bespoke fragment schema), and the
  `health_test.go` cases that want an *unmigrated* database.
  `keyvault_test.go` keeps `sqlite://file::memory:?cache=shared&_fk=1` — a different
  DSN for a reason, not a candidate for a blanket rewrite.
- `docs/ARCHITECTURE.md` §4 amended in place, not appended to: the heading is now
  both engines, the pool difference and the rebinding trap are stated, the cursor
  gotcha is scoped to SQLite with the reason the pattern must stay anyway, and a new
  "double-booking guarantee, per engine" subsection carries the single connection on
  SQLite and the advisory lock on Postgres, with the index's real scope. §17's
  gotcha 1 and one now-conditional aside in §8 follow it.
- `.github/workflows/ci.yml` gains a `postgres` job: `postgres:17` service with a
  `pg_isready` health check, the same steps as `check` minus `svelte-check`, and
  `CALNODE_TEST_POSTGRES_DSN` set for `go test ./...`. Additive — with the variable
  unset nothing about an existing run changes.

## No longer blocked

`migrations/postgres` landed with `feat/postgres-core`, so the `postgres` CI job now
has something to migrate and passes.

## Boundary 5 — the suite actually green on both engines — DONE

Rebased onto the finished `feat/postgres-core`, so `migrations/postgres` exists and
the suite could finally run against PostgreSQL. **15 failures**, four distinct
causes — not the one the first triage suggested.

1. **Constraint classification (7 failures).** Thirteen sites decided 409/400/404
   vs 500 by substring-matching SQLite's English error text. `internal/db` now
   exports `IsUniqueViolation` / `IsCheckViolation` / `IsForeignKeyViolation`,
   matching SQLSTATE 23505 / 23514 / 23503 on Postgres and the message on SQLite.
   Pinned by `constraint_test.go`, which provokes a real violation of each class
   against the real schema on whichever engine is configured, asserts exactly one
   of three predicates matches, and asserts none match an unrelated error.
2. **Engine-dependent boolean expressions (2 failures).** `(user_id = ?) AS owned`
   and `(archived_at IS NOT NULL) AS archived` yield 0/1 on SQLite and a boolean on
   Postgres, which does not scan into the `int` the columns beside them use. Both
   are now `CASE WHEN … THEN 1 ELSE 0 END`, portable and matching the schema's own
   0/1 convention.
3. **`json_extract` in production (1 failure).** `replaceReminderJobs` filtered on
   `json_extract(payload, '$.booking_id')`. No portable spelling exists, so this is
   a second `d.SQL` pair: `payload::json ->> 'booking_id'` on Postgres (the cast is
   needed because the column is TEXT). The reschedule test carried its own copy of
   the same expression, and discarded the `Scan` error, so the real failure showed
   up two seconds later as a time-parsing error.
4. **`ORDER BY rowid` (3 failures).** `webhook_deliveries` had no timestamp of its
   own, so recency was `rowid DESC` — unportable, and not strictly correct on
   SQLite either, since VACUUM may renumber rowids. Migration **00058** adds
   `created_at TEXT NOT NULL DEFAULT ''` to both dirs, the writer binds
   `dbtime.NowMilli()`, and the query is `ORDER BY created_at DESC, id DESC`.
5. **`randomblob` in a gcal test helper (2 failures).** SQLite-only id generator,
   replaced with `uid.New()` like every other row in those tests.

Both suites green in one commit: 28/28 packages on SQLite, 28/28 on PostgreSQL.

## Boundary 6 — constraint codes on BOTH engines — DONE

The first version of `constraint.go` claimed modernc.org/sqlite exposes no error
codes and matched SQLite on message text. That was false: it defines `type Error`
with `Code()`, populated for every constraint class. Measured, not read:

| violation | SQLite `Code()` | PostgreSQL SQLSTATE |
|---|---|---|
| UNIQUE | 2067 | 23505 |
| PRIMARY KEY | **1555** | 23505 |
| CHECK | 275 | 23514 |
| FOREIGN KEY | 787 | 23503 |
| NOT NULL | 1299 | 23502 |

⛔ A PRIMARY KEY collision is **1555, not 2067**, while its message still reads
"UNIQUE constraint failed". The text match caught both by accident;
`Code() == 2067` alone would not. `idempotency_keys.idempotency_key` is a bare
PRIMARY KEY, so the whole idempotent-replay path depends on 1555. Proved by
deleting 1555 and re-running: the new `unique via primary key` subtest fails naming
the code and the table, and `TestCreateBooking_idempotentReplay` returns
`replay: 500`. `IsUniqueViolation` matches both codes; PostgreSQL needed no change.

The text comparison is now a fallback only, for an error arriving without its
driver type attached, and `TestConstraintTextFallback` executes that branch so it
is not dead code. A driver error whose code does not match is a definite no and
does not fall through — falling through would readmit a 1555 by its message after
excluding it by code.

Also fixed: the two intermittent handler failures under `go test ./...` were a flake
in **my** `dbtest` harness, not the application. Handler goroutines are
fire-and-forget (notify hosts, enqueue webhook, enqueue reminders) and outlive the
test body; closing the pool does not stop an in-flight statement; `DROP SCHEMA
CASCADE` needs an exclusive lock on every object and deadlocks against them
(SQLSTATE 40P01). The drop now runs on a pinned connection with `lock_timeout` and
retries within a bounded budget. Three consecutive full PostgreSQL runs clean.

1. **Booleans — resolved.** The Postgres migrations declare them `SMALLINT`, so the
   tree's `= 1` comparisons and `boolToInt(...)` work unchanged. Only *computed*
   boolean expressions in SELECT lists needed a fix (Boundary 5, cause 2).
2. **Text timestamp collation.** Timestamps are `TEXT` and compared
   lexicographically on purpose (the recordings consent window, the job queue's
   `run_at <= ?`). That is byte ordering under SQLite. Under a non-`C` Postgres
   collation, ordering of mixed shapes (a space-separated `run_at` against a
   `T`-separated one) is not guaranteed to match. Worth either `COLLATE "C"` on
   those columns or a deliberate decision that it is safe. **Closed by Boundary 7.**

## Boundary 7 — the last four items — DONE

Open item 2 above, the `RETURNING` clause Boundary 2 left unverified, the bare
`Open` the port had been dragging along, and the hard-coded pool size.

### 1. TEXT timestamps are `COLLATE "C"` — migration 00059

**54 columns across 27 tables**, enumerated from the migrated schema rather than
from memory: every `*_at` (49 of them, including `locked_until`), plus
`availability_rules.start_time`/`end_time` and `availability_overrides.date`/
`start_time`/`end_time`, which hold `HH:MM` and `YYYY-MM-DD` and are ordered as
times too (`ORDER BY day_of_week, start_time`, `ORDER BY date`). One grouped
`ALTER TABLE` per table, so each is rewritten once. The SQLite half is a no-op
file: BINARY *is* memcmp, so there is nothing to pin, and the file exists because
the directories keep one file per version.

Fixed in the schema rather than in the predicates. There are ~20 distinct
lexicographic time comparisons in the tree (`run_at <= ?`, `expires_at > ?`,
`locked_until < ?`, `decided_at BETWEEN`, `start_at`/`end_at` overlap,
`created_at < ?`, several `ORDER BY created_at`); a `COLLATE "C"` clause on each
is 20 chances to forget one, and forgetting is silent.

⚠️ **The load-bearing site is `internal/handler/notetaker.go`**, which writes
`datetime('now')`'s space-separated shape *because* it sorts before any
`T`-separated stamp, which is what makes a notetaker job due immediately. That is
a dependency on byte ordering in production code, not a theoretical one.

**Measured, and it corrects the framing of the open item.** The server this
branch develops against is PostgreSQL 17.11 with `datcollate = en_US.utf8`
(libc provider) — read from `pg_database`, because ⛔ `SHOW lc_collate` no longer
exists (it stopped being a GUC in PostgreSQL 16 and errors with "unrecognized
configuration parameter", so the obvious way to ask is a dead end). The two
shapes the schema actually stores do **not** flip under it:

| pair | en_US.utf8 | memcmp |
|---|---|---|
| `2026-01-01 20:00:00` vs `2026-01-01T10:00:00Z` | space first | space first |

Nor under **any of the other 878 collations installed on the server** — scanned,
not assumed. glibc ignores the space at the primary level but still sorts a digit
before `T`, which happens to agree with memcmp. So a control built only on those
two values would pass with or without the migration, which is exactly the
vacuous green the packet warned about. The control therefore also carries RFC
3339's lower-case `t`/`z` spelling (§5.6 permits it, so an importer or a
third-party API can hand it to us), where the two orders really do differ:

```
plain: 2026-01-01 10:00:00 | 2026-01-01T10:00:00.000Z | 2026-01-01t10:00:00z | 2026-01-01T10:00:00Z
C    : 2026-01-01 10:00:00 | 2026-01-01T10:00:00.000Z | 2026-01-01T10:00:00Z | 2026-01-01t10:00:00z
```

`internal/db/collation_test.go` holds it three ways. (a) An audit over
`information_schema.columns` matching by NAME, so a timestamp column added by a
later migration is caught without anyone remembering — `collation_name` is NULL
for a default-collated column and `'C'` after an explicit `COLLATE`, so the
assertion is exact. (b) The control above, which **skips naming the server's
collation** if the default already orders byte-wise, rather than passing
vacuously. (c) An ordering test on `jobs.run_at` through the worker's real claim
predicate.

Proved failable by dropping `jobs.run_at` from the migration:

```
collation_test.go:79: 1 of 54 timestamp columns are not COLLATE "C":
    jobs.run_at = <database default>
collation_test.go:242: ORDER BY run_at =
    [… 2026-01-01t10:00:00z 2026-01-01T10:00:00Z …]
    want byte order
    [… 2026-01-01T10:00:00Z 2026-01-01t10:00:00z …]
```

### 2. `RETURNING position` — works unchanged on Postgres

No `d.SQL` pair, no rewrite. The clause is the auto-position `INSERT` in
`question_handler.go`, which computes the position inside the statement
(`VALUES (…, (SELECT COALESCE(MAX(position)+1, 0) …)) RETURNING position`) so two
concurrent creates cannot land on the same one.

The new test goes through the HTTP handler on `dbtest.Open(t)`, so it follows the
environment like everything else, and it asserts the value the handler **scanned
from RETURNING** as well as the row left behind: a `RETURNING` that quietly
produced a zero would still leave a correct row, so checking the table alone —
which `TestCreateQuestion_autoPosition` already did — cannot see it. The
explicit-position case is in the same test because the next auto position after a
pinned 9 must be 10, which is what shows the subselect is evaluated by the engine
rather than the sequence being an artifact of insertion order.

Measured: **0, 1, 2, then 9 explicit, then 10 — identical on both engines.**

### 3. `db.Open` deleted

It returned a bare `*sql.DB` "for callers that have not moved to OpenDB yet".
Nothing outside the package's own tests used it, and it could only ever do harm:
statements through that handle are not rebound, so every `?` is a Postgres syntax
error found at runtime, far from the call. It was also the shape most likely to
be copied by whoever added the next call site.

`/readyz` was the one production caller reaching into the exported embedded field
(`db.SchemaReady(ctx, h.db.DB)`). `*db.DB` now carries `SchemaReady` and
`AppliedVersion`, so the handler asks the handle. The package-level functions stay
for the cases that genuinely hold a bare pool — goose's bookkeeping, the tests
that open an unmigrated one — and `db_test.go` passes `database.DB` explicitly,
which says at the call site that the bare pool is deliberate. `TestOpen_inMemory`
became `TestOpenDB_inMemory` rather than staying named after something that no
longer exists. No test asserts the symbol's absence: the build is the proof.

### 4. Pool size is configurable

`DB_MAX_OPEN_CONNS` / `DB_MAX_IDLE_CONNS`, defaults unchanged at 10/5, validated
in `config.PoolFromEnv`: unset, unparsable or non-positive falls back to the
default with a warning (matching `getBool`/`getDuration`, which also refuse to
fail a boot over a typo in an optional knob), and idle above open is clamped —
`database/sql` reduces it silently anyway, so the clamped pair is the honest
description of what the pool will do. ⚠️ The clamp has to apply to the **default**
idle as well: `DB_MAX_OPEN_CONNS=2` alone must give 2/2, not 2/5. A test pins it.

`OpenDB` reads `PoolFromEnv` itself rather than taking the numbers as an
argument, so all five entry points pick them up without five identical edits and
without one of them silently keeping the defaults; `db.WithPool` is the override
for a caller that must not follow the environment. `db` → `config` is not a
cycle (`config` imports only the standard library).

**SQLite stays pinned at 1/1 and ignores both the environment and `WithPool`.**
That is the correctness guarantee, not a preference: the single connection is
what serialises write transactions (it is why `booking.lockHosts` is a no-op
there) and the pragmas are connection-scoped.
`TestOpenDB_sqlitePoolIsNotConfigurable` asserts it with both knobs at 40/20, for
the `:memory:` and file forms.

The idle limit cannot be read back — `sql.DBStats` exposes the open limit only —
so it is measured: four live transactions force four connections, and after
committing them all the pool keeps **1** with `WithPool(4, 1)`.

### Gates

`gofmt -l .` empty, `go vet ./...` clean, **28/28 packages ok on SQLite and
28/28 on PostgreSQL**. Negative control, the same PostgreSQL command with the
password changed to `wrong`: `internal/db` **fails** rather than skipping, with
`failed SASL auth: FATAL: password authentication failed for user "postgres"
(SQLSTATE 28P01)` — including `TestPostgres_idleLimitApplied`, the one new test
whose assertions could conceivably have held without a server.

---

# Multi-tenant mode (`feat/multi-tenant`)

One process, many isolated workspaces. `workspaces` is the tenant root, every
application table carries a `workspace_id`, and PostgreSQL row-level security —
not the query author — is what keeps one workspace out of another's rows.
`MULTI_TENANT` unset must behave exactly as before, on SQLite and on
single-tenant PostgreSQL. That is a gate, not a wish.

## Boundary 1 — config + schema — DONE

### Migration 00060, not 00061

The packet numbered it 00061 on the assumption that `feat/platform-hooks` had
landed 00060. It has not: that branch is not in this clone (no `/v1/auth/sso`
endpoint, no `TRUSTED_PROXY_CIDRS`, no `/metrics`), and the 59 files in each
directory are contiguous. `migrations_internal_test.go` asserts
`target version == file count` and names the failure "a gap or a duplicate
number", so 00061 over 59 files reds an existing gate. 00060 it is;
`knownMigrationCount` moved 59 → 60 with it.

### ⛔ ENABLE / FORCE ROW LEVEL SECURITY is NOT in the migration, and the reason is measured

D2 puts `ENABLE` + `FORCE` + the policy in the migration. `FORCE` makes a
table's policy apply to the table's **owner** as well, and in single-tenant mode
`DATABASE_URL` *is* the owner. Measured against the PostgreSQL 17.11 this branch
develops on, with a `NOBYPASSRLS` owner role and one row present:

| role | RLS | `app.workspace_id` | rows seen |
|---|---|---|---|
| owner (NOBYPASSRLS) | ENABLE + FORCE | unset | **0** |
| superuser | ENABLE + FORCE | unset | 1 |
| owner (NOBYPASSRLS) | ENABLE only | unset | 1 |
| non-owner app role | ENABLE only | unset | **0** |

So an unconditional `FORCE` silently blinds every existing single-tenant
PostgreSQL deployment whose DSN is not a superuser — and **the suite's own DSN
is a superuser, so the test lane would have gone green anyway.** Row 4 is the
other half: `ENABLE` alone already isolates a non-owner role, which is what the
application role is under D4.

The split that keeps both promises: the **policies** live in the migration,
where they are reviewable SQL, and a policy on a table whose RLS is not enabled
is inert (verified, not assumed). The two `ALTER TABLE` lines live in
`db.EnableRLS`, which runs at boot **only when `MULTI_TENANT` is set**, on the
**platform handle** (`DATABASE_ADMIN_URL` — the application role cannot run DDL
in multi-tenant mode at all), is idempotent, and whose failure is
`os.Exit(1)`: without it every policy is inert and the process would come up
looking multi-tenant while separating nothing.
`TestPostgres_rlsIsOffUntilEnabled` pins both halves — off straight after
migrating, on for all 32 tenant tables and none of the 4 exempt ones after
`EnableRLS`, twice.

### The column default is `COALESCE(current_setting(…, true), 'default')`

D1 spells it `current_setting('app.workspace_id')`. The bare form **raises** on
an unset parameter, so it would fail every INSERT in single-tenant mode. The
`missing_ok` form plus `COALESCE` gives `'default'` when nothing is bound, which
is what single-tenant wants, and is never reached in multi-tenant mode because
the handle binds before every statement. It also fails **closed** if a
multi-tenant statement ever escapes that binding: the row would be written as
`'default'` and the policy's `WITH CHECK` compares it against an unset parameter
(NULL, so not true), which refuses the INSERT with SQLSTATE 42501 rather than
letting it land in the wrong tenant. `rls_proof_test.go` asserts exactly that,
including that nothing arrives in the default workspace.

### The tenancy proof, and its negative control

`internal/db/rls_proof_test.go` creates a `calnode_app_<hex>` role with
`NOBYPASSRLS` per test schema, grants it the schema's tables, and opens a handle
as it. ⛔ It **skips loudly** rather than falling back if the role cannot be
created, or reports `rolsuper`/`rolbypassrls`, or owns any table in the schema —
a superuser handle would satisfy every assertion whether the policies existed or
not. Four subtests, both halves required:

- unbound: `SELECT COUNT(*) FROM users` returns **0** of 2
- unbound: INSERT refused with **42501**, and **nothing lands in `default`**
- bound to `ws-a`: sees 1 of 2, reads `a@example.com`, INSERT naming **no**
  `workspace_id` lands as `ws-a`, `b@example.com` invisible
- bound to `ws-a`, naming `ws-b` explicitly: refused with **42501**

`set_config(…, false)` is session-scoped, so every subtest pins one `*sql.Conn`
and writes `$n` directly — statements through a `*sql.Conn` are not rebound.

Proved failable by making `EnableRLS` return early:

```
--- FAIL: TestPostgres_rlsIsolatesAnUnprivilegedRole/unbound_reads_nothing
    rls_proof_test.go:178: an unbound session sees 2 of 2 users; want 0 — an unset app.workspace_id must match no row
--- FAIL: .../unbound_write_is_refused_and_lands_nowhere
    rls_proof_test.go:187: an unbound INSERT succeeded; the policy's WITH CHECK must refuse it
--- FAIL: .../bound_reads_and_writes_exactly_one_workspace
    rls_proof_test.go:215: a session bound to ws-a sees 3 users; want 1
--- FAIL: .../naming_another_workspace_explicitly_is_refused
    rls_proof_test.go:261: writing into another workspace succeeded; WITH CHECK must refuse it
```

### 32 tenant tables, 4 exempt, and a gate against forgetting

`db.TenantTables` / `db.ExemptTables` are the Go copies of the list in the
migration header, and `TestTenancy_tableListsCoverTheSchema` fails if a base
table is in neither — so a table added by a later migration has to be
classified. Exempt: `workspaces` (the root, with its own SELECT-only policy for
the application role), `crypto_keystore` (one DEK per process, D3),
`goose_db_version`, `oauth_clients` (dynamic client registration is per client
*application*; the per-tenant half is `oauth_access_tokens`, which is a tenant
table).

### SQLite's three forced differences

1. **No `REFERENCES workspaces(id)`.** Measured on modernc.org/sqlite with
   `foreign_keys=ON`: `ALTER TABLE t ADD COLUMN workspace_id TEXT NOT NULL
   DEFAULT 'default' REFERENCES ws(id)` is **rejected** — "Cannot add a
   REFERENCES column with non-NULL default value (1)". Rebuilding all 32 tables
   to get a constraint whose only payoff is cascade-on-workspace-delete, in an
   engine that cannot run multi-tenant, is not worth it. The two tables rebuilt
   below omit it too, so the engine is consistent with itself.
2. **No RLS, no policies.** Nothing to express them with, which is why
   `config.Validate` refuses `MULTI_TENANT` without a `postgres://` DSN.
3. **Only the uniqueness that has to MOVE moves.** `idempotency_keys` and
   `meeting_consents` are rebuilt (their PRIMARY KEY changes and SQLite cannot
   ALTER a table constraint); `ux_jobs_type_payload` and `idx_notes_booking` are
   dropped and recreated (plain indexes need no rebuild). `users(email)`,
   `event_types(slug)`, `teams(slug)` and `server_settings`' `id = 1` singleton
   stay exactly as they are: with one workspace, a global unique and a
   `(workspace_id, x)` unique admit precisely the same rows.

### ⚠️ `jobs` keeps a second, workspace-free copy of each partial index

The packet says every partial index on `bookings` and `jobs` gets `workspace_id`
prepended. Correct for `bookings` — every query that uses those indexes now
carries a workspace predicate. **Not** for `jobs`: it is the one table worked
*across* tenants, and the worker's claim and its crash-recovery reaper (B5) run
on the platform handle with no workspace predicate, ordered by `run_at` /
`locked_until` globally. A workspace-leading index cannot serve an ordered global
scan. So both are prepended as instructed **and** `idx_jobs_pending_global`
`(run_at)` and `idx_jobs_running_expired_global` `(locked_until)` are added
alongside. Four small indexes on a table that holds pending work, not history.

### `demo.Reset` had to learn about the tenant root

The Postgres path is one `TRUNCATE … CASCADE` naming every table from
`pg_tables`, which took `workspaces` with it, and the re-seed's first
`server_settings` INSERT then failed with SQLSTATE 23503. `workspaces` is now
held back alongside `goose_db_version`: it is the tenant root, not visitor data,
and in demo mode it holds exactly one constant row. This was the only pre-existing
test the migration broke, on either engine.

### Config

`MULTI_TENANT`, `DATABASE_ADMIN_URL`, `CALNODE_PLATFORM_TOKEN`, and a new
`(*Config).Validate` called from `main` before the database is opened. It is
separate from `Load` because every optional knob in `Load` deliberately falls
back on a typo rather than refusing to boot (`PoolFromEnv`); these are not typos.
Five refusals, each one a combination whose only other outcome is silent:
a non-`postgres://` `DATABASE_URL`, a missing or non-Postgres
`DATABASE_ADMIN_URL`, **the two DSNs being equal** (one role means the
application role owns the tables and every policy is inert against it — the
misconfiguration hardest to notice, because everything works, including reading
other tenants' rows), and `DEMO_MODE` (D13: demo mode periodically wipes the
whole database, which here is every tenant's data).

### Gates

`gofmt -l .` empty, `go vet ./...` clean, `go build ./...` clean.
`go test ./...` **rc=0 on SQLite (28/28 packages)** and **rc=0 on PostgreSQL
(28/28)**.

### Not yet wired (later boundaries, stated so it is not mistaken for done)

`cmd/calnode/main.go` opens the platform handle with a second `db.OpenDB` and
uses it for `Migrate` + `EnableRLS`; Boundary 2 replaces that pair with
`db.OpenPair`. The other entry points (`mcp`, `reset-admin`, `rotate-key`,
`recover-key`) still open one handle from `DATABASE_URL` and are untouched —
they are single-tenant operator tools today, and B3/B6 decide what they become.
