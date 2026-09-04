package db_test

import (
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/db"
)

func TestRebind(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  string
	}{{
		name:  "no placeholders",
		query: `SELECT id FROM users`,
		want:  `SELECT id FROM users`,
	}, {
		name:  "one placeholder",
		query: `SELECT id FROM users WHERE email = ?`,
		want:  `SELECT id FROM users WHERE email = $1`,
	}, {
		name:  "numbered left to right",
		query: `UPDATE users SET name = ?, email = ? WHERE id = ?`,
		want:  `UPDATE users SET name = $1, email = $2 WHERE id = $3`,
	}, {
		name:  "string literal holding a question mark",
		query: `SELECT id FROM event_types WHERE name = 'why?' AND slug = ?`,
		want:  `SELECT id FROM event_types WHERE name = 'why?' AND slug = $1`,
	}, {
		name:  "escaped quote inside a literal does not end it",
		query: `SELECT ? WHERE reason = 'it''s a ?' AND id = ?`,
		want:  `SELECT $1 WHERE reason = 'it''s a ?' AND id = $2`,
	}, {
		name:  "double-quoted identifier",
		query: `SELECT "odd?column" FROM t WHERE id = ?`,
		want:  `SELECT "odd?column" FROM t WHERE id = $1`,
	}, {
		name:  "escaped double quote inside an identifier",
		query: `SELECT "a""?b" FROM t WHERE id = ?`,
		want:  `SELECT "a""?b" FROM t WHERE id = $1`,
	}, {
		name:  "backtick identifier",
		query: "SELECT `weird?` FROM t WHERE id = ?",
		want:  "SELECT `weird?` FROM t WHERE id = $1",
	}, {
		name:  "line comment",
		query: "SELECT id -- what about ?\nFROM t WHERE id = ?",
		want:  "SELECT id -- what about ?\nFROM t WHERE id = $1",
	}, {
		name:  "line comment at end of input",
		query: `SELECT id FROM t WHERE id = ? -- trailing ?`,
		want:  `SELECT id FROM t WHERE id = $1 -- trailing ?`,
	}, {
		name:  "block comment",
		query: `SELECT /* ? not a param ? */ id FROM t WHERE id = ?`,
		want:  `SELECT /* ? not a param ? */ id FROM t WHERE id = $1`,
	}, {
		name:  "nested block comment",
		query: `SELECT /* outer /* inner ? */ still ? */ id FROM t WHERE id = ?`,
		want:  `SELECT /* outer /* inner ? */ still ? */ id FROM t WHERE id = $1`,
	}, {
		name:  "division is not a comment",
		query: `SELECT price_cents / 100 FROM event_types WHERE id = ?`,
		want:  `SELECT price_cents / 100 FROM event_types WHERE id = $1`,
	}, {
		name:  "negative number is not a comment",
		query: `SELECT id FROM t WHERE n = -1 AND id = ?`,
		want:  `SELECT id FROM t WHERE n = -1 AND id = $1`,
	}, {
		name:  "unterminated literal swallows the rest",
		query: `SELECT id FROM t WHERE s = 'oops ? and id = ?`,
		want:  `SELECT id FROM t WHERE s = 'oops ? and id = ?`,
	}, {
		name:  "unterminated block comment swallows the rest",
		query: `SELECT id FROM t /* oops ? and id = ?`,
		want:  `SELECT id FROM t /* oops ? and id = ?`,
	}, {
		name:  "already rebound is left alone",
		query: `SELECT id FROM t WHERE id = $1`,
		want:  `SELECT id FROM t WHERE id = $1`,
	}, {
		name:  "multi-line statement with a comment block",
		query: "-- +goose Up\nINSERT INTO t (a, b) VALUES (?, ?)\n  -- ? in a comment\n  ON CONFLICT DO NOTHING",
		want:  "-- +goose Up\nINSERT INTO t (a, b) VALUES ($1, $2)\n  -- ? in a comment\n  ON CONFLICT DO NOTHING",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := db.Rebind(tt.query); got != tt.want {
				t.Errorf("Rebind(%q)\n got %q\nwant %q", tt.query, got, tt.want)
			}
		})
	}
}

// TestRebind_manyPlaceholders covers the two-digit boundary: a naive
// implementation that scanned for "$1" or replaced in a fixed order would go
// wrong at ten.
func TestRebind_manyPlaceholders(t *testing.T) {
	const n = 12
	query := "INSERT INTO t VALUES (" + strings.Repeat("?, ", n-1) + "?)"

	got := db.Rebind(query)

	want := "INSERT INTO t VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)"
	if got != want {
		t.Errorf("Rebind(%q)\n got %q\nwant %q", query, got, want)
	}
}

func TestDialectRebind(t *testing.T) {
	const query = `SELECT id FROM users WHERE email = ? AND is_admin = ?`

	if got := db.DialectSQLite.Rebind(query); got != query {
		t.Errorf("DialectSQLite.Rebind rewrote the query: %q", got)
	}

	want := `SELECT id FROM users WHERE email = $1 AND is_admin = $2`
	if got := db.DialectPostgres.Rebind(query); got != want {
		t.Errorf("DialectPostgres.Rebind = %q; want %q", got, want)
	}
}

func TestDialectSQL(t *testing.T) {
	const (
		sqlite   = `UPDATE server_settings SET updated_at = datetime('now') WHERE id = 1`
		postgres = `UPDATE server_settings SET updated_at = to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') WHERE id = 1`
	)

	if got := db.DialectSQLite.SQL(sqlite, postgres); got != sqlite {
		t.Errorf("DialectSQLite.SQL = %q; want the sqlite statement", got)
	}
	if got := db.DialectPostgres.SQL(sqlite, postgres); got != postgres {
		t.Errorf("DialectPostgres.SQL = %q; want the postgres statement", got)
	}
}

func TestDialectString(t *testing.T) {
	if got := db.DialectSQLite.String(); got != "sqlite" {
		t.Errorf("DialectSQLite.String() = %q; want %q", got, "sqlite")
	}
	if got := db.DialectPostgres.String(); got != "postgres" {
		t.Errorf("DialectPostgres.String() = %q; want %q", got, "postgres")
	}
}
