package db

import (
	"strconv"
	"strings"
)

// Rebind rewrites the ? placeholders in query to PostgreSQL's $1…$n form,
// numbering them left to right so the caller's argument order is unchanged.
//
// It is a small lexer rather than a string replace because a ? inside a string
// literal, a quoted identifier or a comment is data, not a placeholder.
// Renumbering one of those corrupts the statement, and the corruption surfaces
// at runtime — as a wrong result or a confusing type error — a long way from the
// code that caused it.
//
// Not recognised, because Calnode's SQL contains none of them and guessing would
// be worse than saying so: PostgreSQL escape strings (E'...', where a backslash
// escapes the closing quote), dollar-quoted bodies ($tag$…$tag$), and
// MSSQL-style [bracketed] identifiers. Statements using those must be written per
// dialect with Dialect.SQL and their own $n.
func Rebind(query string) string {
	// Overwhelmingly the common case for DDL and for already-converted SQL.
	if !strings.Contains(query, "?") {
		return query
	}

	var b strings.Builder
	b.Grow(len(query) + 8)

	n := 0
	for i := 0; i < len(query); {
		switch c := query[i]; {
		case c == '\'' || c == '"' || c == '`':
			// A quoted run is copied verbatim: single quotes are string
			// literals, and both double quotes and backticks are identifier
			// quotes SQLite accepts.
			end := endOfQuoted(query, i)
			b.WriteString(query[i:end])
			i = end
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			end := endOfLineComment(query, i)
			b.WriteString(query[i:end])
			i = end
		case c == '/' && i+1 < len(query) && query[i+1] == '*':
			end := endOfBlockComment(query, i)
			b.WriteString(query[i:end])
			i = end
		case c == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}

	return b.String()
}

// endOfQuoted returns the index just past the quoted run starting at i, where
// query[i] is the opening quote. A doubled quote is an escaped quote and does not
// close the run — that is SQL's only escape under both engines' default settings.
// An unterminated run extends to the end of the string, which hands the malformed
// statement to the engine to reject instead of mangling the remainder.
func endOfQuoted(query string, i int) int {
	quote := query[i]
	for j := i + 1; j < len(query); j++ {
		if query[j] != quote {
			continue
		}
		if j+1 < len(query) && query[j+1] == quote {
			j++ // skip the escaped quote; the loop's j++ skips its partner
			continue
		}
		return j + 1
	}
	return len(query)
}

// endOfLineComment returns the index of the newline ending the -- comment at i,
// so the newline itself is copied by the caller and line structure survives.
func endOfLineComment(query string, i int) int {
	if k := strings.IndexByte(query[i:], '\n'); k >= 0 {
		return i + k
	}
	return len(query)
}

// endOfBlockComment returns the index just past the /* */ comment at i. Nesting
// is honoured: PostgreSQL nests block comments, so treating the first */ as the
// end would drop back into "SQL" while still inside a comment.
func endOfBlockComment(query string, i int) int {
	depth := 0
	for j := i; j+1 < len(query); {
		switch {
		case query[j] == '/' && query[j+1] == '*':
			depth++
			j += 2
		case query[j] == '*' && query[j+1] == '/':
			depth--
			j += 2
			if depth == 0 {
				return j
			}
		default:
			j++
		}
	}
	return len(query)
}
