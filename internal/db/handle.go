package db

import (
	"context"
	"database/sql"
)

// DB is a *sql.DB that knows its dialect and rebinds placeholders.
//
// Every query method takes the portable ? form and rewrites it for the engine in
// use, so the rest of Calnode writes one statement per query no matter which
// database it runs on. Behaviour on SQLite is identical to using *sql.DB
// directly: Rebind is a no-op there.
//
// The embedded *sql.DB is exported on purpose — pool tuning, Ping, Close and
// anything that must hand a plain *sql.DB to a library (goose here, Litestream
// in DEPLOY.md) reach it as h.DB. Statements issued through that field, or
// through a *sql.Conn from the promoted Conn method, are NOT rebound; use the
// wrapper's own methods unless you are deliberately writing engine-specific SQL.
type DB struct {
	*sql.DB
	dialect Dialect
}

// Dialect reports which engine this handle is talking to, for the few callers
// that must branch on it.
func (h *DB) Dialect() Dialect { return h.dialect }

// Rebind converts a ?-placeholder statement for this handle's dialect. Useful
// when a caller builds SQL dynamically and hands it to something other than the
// methods below.
func (h *DB) Rebind(query string) string { return h.dialect.Rebind(query) }

func (h *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return h.DB.Query(h.dialect.Rebind(query), args...)
}

func (h *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return h.DB.QueryContext(ctx, h.dialect.Rebind(query), args...)
}

func (h *DB) QueryRow(query string, args ...any) *sql.Row {
	return h.DB.QueryRow(h.dialect.Rebind(query), args...)
}

func (h *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return h.DB.QueryRowContext(ctx, h.dialect.Rebind(query), args...)
}

func (h *DB) Exec(query string, args ...any) (sql.Result, error) {
	return h.DB.Exec(h.dialect.Rebind(query), args...)
}

func (h *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return h.DB.ExecContext(ctx, h.dialect.Rebind(query), args...)
}

// Prepare and PrepareContext rebind at prepare time, so the returned *sql.Stmt
// needs no wrapper of its own.
func (h *DB) Prepare(query string) (*sql.Stmt, error) {
	return h.DB.Prepare(h.dialect.Rebind(query))
}

func (h *DB) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return h.DB.PrepareContext(ctx, h.dialect.Rebind(query))
}

// Begin and BeginTx return a *Tx rather than a *sql.Tx: a transaction that
// silently stopped rebinding would be the easiest way to reintroduce ? into a
// Postgres statement.
func (h *DB) Begin() (*Tx, error) {
	tx, err := h.DB.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, dialect: h.dialect}, nil
}

func (h *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	tx, err := h.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, dialect: h.dialect}, nil
}

// Tx is a *sql.Tx with the same rebinding behaviour as DB. Commit and Rollback
// are the embedded ones — they carry no SQL text.
type Tx struct {
	*sql.Tx
	dialect Dialect
}

// Dialect reports which engine this transaction is running against.
func (t *Tx) Dialect() Dialect { return t.dialect }

// Rebind converts a ?-placeholder statement for this transaction's dialect.
func (t *Tx) Rebind(query string) string { return t.dialect.Rebind(query) }

func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.Tx.Query(t.dialect.Rebind(query), args...)
}

func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.Tx.QueryContext(ctx, t.dialect.Rebind(query), args...)
}

func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return t.Tx.QueryRow(t.dialect.Rebind(query), args...)
}

func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.Tx.QueryRowContext(ctx, t.dialect.Rebind(query), args...)
}

func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.Tx.Exec(t.dialect.Rebind(query), args...)
}

func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, t.dialect.Rebind(query), args...)
}

func (t *Tx) Prepare(query string) (*sql.Stmt, error) {
	return t.Tx.Prepare(t.dialect.Rebind(query))
}

func (t *Tx) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return t.Tx.PrepareContext(ctx, t.dialect.Rebind(query))
}
