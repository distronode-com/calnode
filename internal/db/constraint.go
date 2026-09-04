package db

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Constraint violations are the one class of database error Calnode routinely acts
// on rather than just reporting: a duplicate slug is a 409, an out-of-range value is
// a 400, a dangling reference is a 404. Deciding which is which used to be a
// substring match on SQLite's English message, which is invisible to every gate and
// silently degrades to a 500 on PostgreSQL.
//
// PostgreSQL is matched on SQLSTATE because that is the only stable handle: the
// message text is localised by the server's lc_messages, so a server running in
// German would defeat any text match. SQLite has no error codes exposed through
// modernc.org/sqlite's error value, so its half stays a text match — which is what
// it always was, and it is pinned by a test that provokes a real violation.
const (
	pgUniqueViolation     = "23505"
	pgCheckViolation      = "23514"
	pgForeignKeyViolation = "23503"
)

// SQLite's messages for the same three classes.
const (
	sqliteUniqueViolation     = "UNIQUE constraint failed"
	sqliteCheckViolation      = "CHECK constraint failed"
	sqliteForeignKeyViolation = "FOREIGN KEY constraint failed"
)

// IsUniqueViolation reports whether err is a unique-constraint violation — a
// duplicate slug, a replayed idempotency key, a second booking at one host's exact
// start time.
func IsUniqueViolation(err error) bool {
	return violates(err, pgUniqueViolation, sqliteUniqueViolation)
}

// IsCheckViolation reports whether err is a CHECK-constraint violation, i.e. a value
// outside the set the column allows. Callers turn this into a 400, since the only way
// to reach it is a request carrying a value the handler did not validate.
func IsCheckViolation(err error) bool {
	return violates(err, pgCheckViolation, sqliteCheckViolation)
}

// IsForeignKeyViolation reports whether err is a foreign-key violation — a reference
// to a row that does not exist, or a delete that would orphan one.
func IsForeignKeyViolation(err error) bool {
	return violates(err, pgForeignKeyViolation, sqliteForeignKeyViolation)
}

// violates is the shared shape: SQLSTATE when the error came from PostgreSQL, the
// message otherwise.
//
// A *pgconn.PgError whose code does NOT match returns false rather than falling
// through to the text comparison. Falling through would let a PostgreSQL error be
// classified by whether its message happened to contain an English phrase from
// another engine, which is exactly the fragility being removed here.
func violates(err error, sqlstate, sqliteText string) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == sqlstate
	}
	return strings.Contains(err.Error(), sqliteText)
}
