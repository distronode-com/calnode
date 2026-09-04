// Package dbtime formats "now" for Calnode's TEXT timestamp columns.
//
// Those columns used to be filled by the engine, with datetime('now') or
// strftime('%Y-%m-%dT%H:%M:%fZ','now'). Neither function exists in PostgreSQL, so
// the value is computed here and bound as an ordinary parameter instead: one
// statement that both engines accept, and a timestamp a test can control by
// comparing against a value it computed itself.
//
// The two layouts are the two shapes already in the schema, kept byte-identical to
// what SQLite wrote before. Normalising them to one would be tidier and wrong: the
// stored text is compared lexicographically (recordings scope consents to a time
// window that way) and handed to clients verbatim (GET /v1/notes/{id} returns
// updated_at), so a shape change would silently alter both.
package dbtime

import "time"

const (
	// DateTime is SQLite's datetime('now') output: "2006-01-02 15:04:05", UTC,
	// second resolution, no zone suffix.
	DateTime = "2006-01-02 15:04:05"

	// RFC3339Milli is SQLite's strftime('%Y-%m-%dT%H:%M:%fZ','now') output:
	// RFC 3339 with exactly three fractional digits. %f is "SS.SSS", so the
	// milliseconds are always present, including a trailing zero.
	RFC3339Milli = "2006-01-02T15:04:05.000Z"
)

// Now returns the current UTC time in the DateTime layout — the replacement for
// datetime('now').
func Now() string { return time.Now().UTC().Format(DateTime) }

// NowMilli returns the current UTC time in the RFC3339Milli layout — the
// replacement for strftime('%Y-%m-%dT%H:%M:%fZ','now').
func NowMilli() string { return time.Now().UTC().Format(RFC3339Milli) }
