package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"nft-forward/internal/db"
)

// This file is a database driver that can fail one chosen statement, and the
// means to serve a single request through it.
//
// A request meets the database as a sequence of statements. Failing the k-th
// of them, for every k, puts the handler through each of its "this query
// failed" branches one at a time — including the ones a whole-table fault
// cannot reach, because the same table is read earlier in the request (the
// second lookup of a user, after authentication has read the first) or
// because they sit behind a write that has to succeed first.

const faultDriverName = "sqlite-statement-fault"

var errInjectedFault = errors.New("injected fault: this statement was made to fail")

// statementScript watches the statements one goroutine sends to one database
// and fails the failAt-th of them (1-based; 0 fails none).
//
// Only the watched goroutine counts. database/sql calls the driver on the
// goroutine that called Exec or Query, so with the request served on the
// watched goroutine these are the handler's own statements, in the order it
// issued them. Work a handler hands to another goroutine (a dispatch to a
// node) is neither counted nor failed; it would otherwise be interleaved at
// random and make "the k-th statement" mean a different one on every run.
type statementScript struct {
	mu     sync.Mutex
	gid    uint64 // watched goroutine; 0 while not armed
	failAt int
	seen   []string
	failed string
	// failedIn is the bestEffortWork the failed statement belonged to, "" if
	// it was the request's own.
	failedIn string
	// failedStack is the functions on the stack that issued the failed
	// statement, innermost first.
	failedStack []string
	// own counts the seen statements that are not authenticationWork, and
	// auth is the authenticationWork functions that issued the others.
	own  int
	auth map[string]bool
}

// bestEffortWork names the functions whose database work is not part of what
// a request is answered for, with the reason. A statement counts as theirs
// when one of them is on the stack that issued it. Such a statement may fail
// and leave the reply as it would have been; any other statement may not.
var bestEffortWork = map[string]string{
	"nft-forward/internal/server.(*Server).dispatchToNode": "pushing the stored rules to a node. The reply is about what was stored; " +
		"a push that fails is recorded on the node (last_error), logged, and made again when the node reconnects",
	"nft-forward/internal/db.WriteAudit":         "the audit trail; an entry that cannot be written is logged",
	"nft-forward/internal/db.TouchAPITokenUsage": "when a token was last used",
}

// authenticationWork names the lookups of who is asking. Their statements
// are issued for every request to an authenticated route, whatever the
// route; the rest are the handler's own.
var authenticationWork = map[string]string{
	"nft-forward/internal/db.GetSessionUser":     "the session cookie's user",
	"nft-forward/internal/db.GetUserByAPIToken":  "the bearer token's user",
	"nft-forward/internal/db.TouchAPITokenUsage": "when the token was last used",
}

// callingStack returns the functions on the calling stack, innermost first.
func callingStack() []string {
	var pcs [64]uintptr
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs[:])])
	var stack []string
	for {
		frame, more := frames.Next()
		stack = append(stack, frame.Function)
		if !more {
			return stack
		}
	}
}

// firstOf returns the first function on stack that is a key of among, or "".
func firstOf[V any](stack []string, among map[string]V) string {
	for _, fn := range stack {
		if _, ok := among[fn]; ok {
			return fn
		}
	}
	return ""
}

// statementScripts maps a database file to the script for its connections.
var statementScripts sync.Map

func (sc *statementScript) arm(failAt int) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.gid, sc.failAt, sc.seen, sc.failed, sc.failedIn, sc.failedStack = goroutineID(), failAt, nil, "", "", nil
	sc.own, sc.auth = 0, map[string]bool{}
}

// disarm stops watching.
func (sc *statementScript) disarm() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.gid = 0
}

func (sc *statementScript) before(stmt string) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.gid == 0 || sc.gid != goroutineID() {
		return nil
	}
	sc.seen = append(sc.seen, strings.Join(strings.Fields(stmt), " "))
	stack := callingStack()
	if fn := firstOf(stack, authenticationWork); fn != "" {
		sc.auth[fn] = true
	} else {
		sc.own++
	}
	if len(sc.seen) == sc.failAt {
		sc.failed, sc.failedStack = sc.seen[len(sc.seen)-1], stack
		sc.failedIn = firstOf(stack, bestEffortWork)
		return errInjectedFault
	}
	return nil
}

// goroutineID reads the calling goroutine's id off its stack header
// ("goroutine 18 [running]:"). The runtime offers no other way to ask.
func goroutineID() uint64 {
	var buf [64]byte
	fields := strings.Fields(string(buf[:runtime.Stack(buf[:], false)]))
	id, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		panic("unexpected stack header: " + string(buf[:]))
	}
	return id
}

// faultDriver hands out the real SQLite driver's connections, wrapped.
type faultDriver struct{ real driver.Driver }

func init() {
	plain, err := sql.Open("sqlite", "")
	if err != nil {
		panic(err)
	}
	sql.Register(faultDriverName, faultDriver{plain.Driver()})
	plain.Close()
}

func (fd faultDriver) Open(dsn string) (driver.Conn, error) {
	path, _, _ := strings.Cut(dsn, "?")
	sc, ok := statementScripts.Load(path)
	if !ok {
		return nil, fmt.Errorf("no statement script registered for %s", path)
	}
	c, err := fd.real.Open(dsn)
	if err != nil {
		return nil, err
	}
	fc := &faultConn{Conn: c, script: sc.(*statementScript)}
	// database/sql uses these when a connection offers them. They are the
	// paths the wrapper intercepts, so the real connection has to offer them
	// too; otherwise statements would go through Prepare, unseen.
	if fc.execer, ok = c.(driver.ExecerContext); !ok {
		return nil, errors.New("the SQLite connection has no ExecContext")
	}
	if fc.queryer, ok = c.(driver.QueryerContext); !ok {
		return nil, errors.New("the SQLite connection has no QueryContext")
	}
	if fc.beginner, ok = c.(driver.ConnBeginTx); !ok {
		return nil, errors.New("the SQLite connection has no BeginTx")
	}
	return fc, nil
}

type faultConn struct {
	driver.Conn
	script   *statementScript
	execer   driver.ExecerContext
	queryer  driver.QueryerContext
	beginner driver.ConnBeginTx
}

func (c *faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.script.before(query); err != nil {
		return nil, err
	}
	return c.execer.ExecContext(ctx, query, args)
}

func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.script.before(query); err != nil {
		return nil, err
	}
	return c.queryer.QueryContext(ctx, query, args)
}

func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.script.before("BEGIN"); err != nil {
		return nil, err
	}
	tx, err := c.beginner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, script: c.script}, nil
}

// CheckNamedValue keeps the real connection's say over argument types; without
// it database/sql would apply its default conversion to the wrapper.
func (c *faultConn) CheckNamedValue(nv *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

type faultTx struct {
	driver.Tx
	script *statementScript
}

func (tx *faultTx) Commit() error {
	if err := tx.script.before("COMMIT"); err != nil {
		// A commit that fails commits nothing, and the connection has to be
		// left usable for the next statement.
		tx.Tx.Rollback()
		return err
	}
	return tx.Tx.Commit()
}

// A sweepCall is one request to the JSON API as one of the seeded identities.
type sweepCall struct {
	method, pattern, url, who string
	body                      any
	// want is the status of the reply when nothing fails; 0 means 200.
	want int
}

func (c sweepCall) String() string { return c.who + " " + c.method + " " + c.url }

func (c sweepCall) wantStatus() int {
	if c.want == 0 {
		return http.StatusOK
	}
	return c.want
}

// A sweepRun is what one call did.
type sweepRun struct {
	status int
	body   string
	stmts  []string // the statements the request issued, in order
	own    int      // how many of them are the handler's own (statementScript.own)
	auth   map[string]bool
	failed string // the statement that was made to fail; "" if none was
	// failedIn is the bestEffortWork the failed statement belonged to; "" if
	// it was the request's own.
	failedIn string
	// changed reports whether the request left the stored state different
	// from what it found (see storedState).
	changed bool
}

// runSweepCall serves the call from a fresh copy of the seed database at path,
// failing the failAt-th statement of the request (0: none).
// openFaulted writes a fresh copy of the seeded database to path and opens a
// server on it through the fault driver, with no periodic work running. The
// script fails nothing until it is armed. release closes the database.
func openFaulted(seed faultSeed, path string) (s *Server, script *statementScript, release func(), err error) {
	if err := os.WriteFile(path, seed.image, 0o600); err != nil {
		return nil, nil, nil, err
	}
	script = &statementScript{}
	statementScripts.Store(path, script)
	d, err := db.OpenDriver(faultDriverName, path)
	if err != nil {
		statementScripts.Delete(path)
		return nil, nil, nil, err
	}
	release = func() {
		d.Close()
		statementScripts.Delete(path)
	}
	if s, err = newServer(d); err != nil {
		release()
		return nil, nil, nil, err
	}
	return s, script, release, nil
}

func runSweepCall(seed faultSeed, path string, call sweepCall, failAt int) (sweepRun, error) {
	s, script, release, err := openFaulted(seed, path)
	if err != nil {
		return sweepRun{}, err
	}
	defer release()
	d := s.DB
	before, err := storedState(d)
	if err != nil {
		return sweepRun{}, err
	}
	var payload []byte
	if call.body != nil {
		if payload, err = json.Marshal(call.body); err != nil {
			return sweepRun{}, err
		}
	}
	req := httptest.NewRequest(call.method, call.url, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	seed.authenticate(req, call.who)
	rec := httptest.NewRecorder()
	router := s.Router()
	script.arm(failAt)
	router.ServeHTTP(rec, req)
	script.disarm()
	after, err := storedState(d)
	if err != nil {
		return sweepRun{}, err
	}
	return sweepRun{
		status: rec.Code, body: strings.TrimSpace(rec.Body.String()),
		stmts: script.seen, own: script.own, auth: script.auth, failed: script.failed, failedIn: script.failedIn,
		changed: before != after,
	}, nil
}

// notStoredState lists the tables and columns storedState leaves out, and why
// each is not part of "what the request saved".
var notStoredState = map[string]string{
	"schema_migrations":       "written by Open only",
	"audit_logs":              "a record that something was asked for; it says nothing about what is in effect",
	"api_tokens.last_used_at": "when the token was last used, written on every authenticated call",
	"nodes.last_error":        "the outcome of the last push to the node, written by dispatchToNode",
	"nodes.last_warning":      "as last_error",
	"nodes.last_apply_at":     "as last_error",
}

// storedState digests everything the database holds except notStoredState.
// Two equal digests mean a request saved nothing.
func storedState(d *sql.DB) (string, error) {
	rows, err := d.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return "", err
	}
	// Every notStoredState entry has to name something that exists: one
	// that names nothing leaves out nothing, and nobody would notice.
	unused := map[string]bool{}
	for name := range notStoredState {
		unused[name] = true
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return "", err
		}
		if _, skip := notStoredState[name]; skip {
			delete(unused, name)
		} else {
			tables = append(tables, name)
		}
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	h := sha256.New()
	for _, table := range tables {
		rows, err := d.Query(`SELECT * FROM ` + table)
		if err != nil {
			return "", err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return "", err
		}
		var lines []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return "", err
			}
			for i, col := range cols {
				if _, skip := notStoredState[table+"."+col]; skip {
					vals[i] = nil
				}
			}
			lines = append(lines, fmt.Sprintf("%q", vals))
		}
		for _, col := range cols {
			delete(unused, table+"."+col)
		}
		if err := rows.Close(); err != nil {
			return "", err
		}
		sort.Strings(lines)
		fmt.Fprintf(h, "%s %d\n%s\n", table, len(lines), strings.Join(lines, "\n"))
	}
	for name := range unused {
		return "", fmt.Errorf("notStoredState names %q, which is not a table or column of the database", name)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
