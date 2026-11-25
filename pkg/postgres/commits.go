package postgres

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

// Notification is a pg_notify call that has been committed.
type Notification struct {
	Channel string
	Payload string
}

// CommitTracer tells the process, right after a transaction commits, that something was
// written and which notifications were sent. It replaces two things that kept the database
// awake around the clock: workers that polled for new rows, and a held LISTEN connection that
// reconnected whenever the database suspended.
//
// Nothing here queries the database. It watches the statements this process runs, so it
// hears about its own writes. Writes made by another instance are caught by the workers'
// slow sweep.
//
// What counts as a write is an INSERT, UPDATE or DELETE that changed at least one row, seen
// once the transaction has committed (a statement outside a transaction is committed at once;
// one inside waits for COMMIT, and a rollback forgets it). A worker's claim that finds nothing
// changes no row, so it never wakes anyone and the chain of wake-ups ends.
type CommitTracer struct {
	// OnWrite is called once per commit that wrote rows, with the tables it wrote to (sorted,
	// without duplicates). The names come from the statements, so a table written through a
	// function or a trigger is not listed.
	OnWrite func(tables []string)

	// OnNotify is called for each pg_notify of a commit, in order.
	OnNotify func(Notification)

	mu      sync.Mutex
	pending map[*pgx.Conn]*pendingCommit
}

type pendingCommit struct {
	wrote  bool
	tables map[string]struct{}
	notes  []Notification
}

type (
	queryKey struct{}
	query    struct {
		sql  string
		args []any
	}
)

// _notify matches `SELECT pg_notify('channel', $1)`, the form the store uses.
var _notify = regexp.MustCompile(`^\s*SELECT pg_notify\('([A-Za-z0-9_]+)',\s*\$1\)\s*$`)

// _writes finds the table of each INSERT INTO, UPDATE and DELETE FROM in a statement, also
// inside a WITH. "DO UPDATE SET" matches too and yields "set", which is dropped.
var _writes = regexp.MustCompile(`(?i)\b(?:insert\s+into|update|delete\s+from)\s+(?:only\s+)?"?([a-z_][a-z0-9_]*)"?`)

var (
	_ pgx.QueryTracer = (*CommitTracer)(nil)
	_ pgx.BatchTracer = (*CommitTracer)(nil)
)

// TraceQueryStart remembers the statement for TraceQueryEnd.
func (t *CommitTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, queryKey{}, query{sql: data.SQL, args: data.Args})
}

// TraceQueryEnd records what the statement did and flushes when the connection is outside a transaction.
func (t *CommitTracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	q, _ := ctx.Value(queryKey{}).(query)

	if data.Err == nil {
		t.observe(conn, q.sql, q.args, data.CommandTag.String())
	}

	t.settle(conn, data.CommandTag.String())
}

// TraceBatchStart does nothing: every statement of a batch is seen in TraceBatchQuery.
func (t *CommitTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

// TraceBatchQuery records what one statement of a batch did.
func (t *CommitTracer) TraceBatchQuery(_ context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	if data.Err == nil {
		t.observe(conn, data.SQL, data.Args, data.CommandTag.String())
	}
}

// TraceBatchEnd flushes when the batch left the connection outside a transaction.
func (t *CommitTracer) TraceBatchEnd(_ context.Context, conn *pgx.Conn, _ pgx.TraceBatchEndData) {
	t.settle(conn, "")
}

func (t *CommitTracer) observe(conn *pgx.Conn, sql string, args []any, tag string) {
	if m := _notify.FindStringSubmatch(sql); m != nil && len(args) == 1 {
		if payload, ok := args[0].(string); ok {
			t.update(conn, func(p *pendingCommit) { p.notes = append(p.notes, Notification{Channel: m[1], Payload: payload}) })
		}

		return
	}

	if changedRows(tag) {
		names := writtenTables(sql)

		t.update(conn, func(p *pendingCommit) {
			p.wrote = true

			for _, n := range names {
				if p.tables == nil {
					p.tables = map[string]struct{}{}
				}

				p.tables[n] = struct{}{}
			}
		})
	}
}

// settle decides what happens to the connection's pending work after a statement.
func (t *CommitTracer) settle(conn *pgx.Conn, tag string) {
	switch {
	case tag == "ROLLBACK":
		t.forget(conn)
	case conn.PgConn().TxStatus() == 'I':
		t.flush(conn)
	}
}

func (t *CommitTracer) update(conn *pgx.Conn, fn func(*pendingCommit)) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.pending == nil {
		t.pending = map[*pgx.Conn]*pendingCommit{}
	}

	p := t.pending[conn]
	if p == nil {
		p = &pendingCommit{}
		t.pending[conn] = p
	}

	fn(p)
}

func (t *CommitTracer) take(conn *pgx.Conn) *pendingCommit {
	t.mu.Lock()
	defer t.mu.Unlock()

	p := t.pending[conn]
	delete(t.pending, conn)

	return p
}

func (t *CommitTracer) flush(conn *pgx.Conn) {
	p := t.take(conn)
	if p == nil {
		return
	}

	if t.OnNotify != nil {
		for _, n := range p.notes {
			t.OnNotify(n)
		}
	}

	if p.wrote && t.OnWrite != nil {
		tables := make([]string, 0, len(p.tables))
		for n := range p.tables {
			tables = append(tables, n)
		}

		sort.Strings(tables)

		t.OnWrite(tables)
	}
}

func (t *CommitTracer) forget(conn *pgx.Conn) { _ = t.take(conn) }

// changedRows reports an INSERT, UPDATE or DELETE that changed at least one row.
func changedRows(tag string) bool {
	var verb string

	switch {
	case len(tag) > 7 && tag[:7] == "INSERT ":
		verb = "INSERT"
	case len(tag) > 7 && tag[:7] == "UPDATE ":
		verb = "UPDATE"
	case len(tag) > 7 && tag[:7] == "DELETE ":
		verb = "DELETE"
	default:
		return false
	}

	// INSERT tags are "INSERT <oid> <rows>", the others "<VERB> <rows>".
	last := tag[len(verb)+1:]
	for i := len(last) - 1; i >= 0; i-- {
		if last[i] == ' ' {
			last = last[i+1:]

			break
		}
	}

	return last != "" && last != "0"
}

// writtenTables lists the tables a statement writes to.
func writtenTables(sql string) []string {
	var out []string

	for _, m := range _writes.FindAllStringSubmatch(sql, -1) {
		if name := strings.ToLower(m[1]); name != "set" {
			out = append(out, name)
		}
	}

	return out
}
