package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

// loseTable makes every statement that touches the table fail, while the rest
// of the database keeps working.
func loseTable(t *testing.T, d *sql.DB, table string) {
	t.Helper()
	if _, err := d.Exec(`ALTER TABLE ` + table + ` RENAME TO ` + table + `_gone`); err != nil {
		t.Fatalf("rename %s: %v", table, err)
	}
}

// cookieDo serves one JSON request as the holder of the cookie and returns the
// reply whatever its status.
func cookieDo(t *testing.T, s *Server, cookie *http.Cookie, method, url string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, url, bytes.NewReader(b))
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func clearsSessionCookie(rec *httptest.ResponseRecorder) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

// A session lookup that fails says nothing about the cookie. Only "no such
// session" may end it: answered with 401 and a cleared cookie, a database
// hiccup would log every user out.
func TestSessionLookupFailureIsNotALogout(t *testing.T) {
	d := openDB(t)
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	// Control: a cookie the server has never issued is a logout.
	rec := cookieDo(t, s, &http.Cookie{Name: sessionCookie, Value: "never-issued"}, "GET", "/api/nodes", nil)
	if rec.Code != http.StatusUnauthorized || !clearsSessionCookie(rec) {
		t.Fatalf("unknown session: want 401 with the cookie cleared, got %d cleared=%v", rec.Code, clearsSessionCookie(rec))
	}

	loseTable(t, d, "sessions")
	rec = cookieDo(t, s, admin, "GET", "/api/nodes", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed session lookup: want 500, got %d %s", rec.Code, rec.Body)
	}
	if clearsSessionCookie(rec) {
		t.Fatal("failed session lookup cleared the session cookie")
	}
}

// Same for API tokens: a caller told its token is invalid has reason to throw
// it away.
func TestTokenLookupFailureIsNotAnInvalidToken(t *testing.T) {
	d := openDB(t)
	_, tok := v1AdminToken(t, d, db.TokenScopeRead)
	s, _ := New(d)

	if rec := v1Do(t, s, "GET", "/api/v1/info", "not-a-token", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token: want 401, got %d", rec.Code)
	}
	loseTable(t, d, "api_tokens")
	if rec := v1Do(t, s, "GET", "/api/v1/info", tok, nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed token lookup: want 500, got %d %s", rec.Code, rec.Body)
	}
}

// Changing a password ends every other session of the user and keeps the one
// that made the change.
func TestChangePasswordKeepsOnlyTheCallersSession(t *testing.T) {
	d := openDB(t)
	admin := loginAsAdmin(t, d)
	var uid int64
	if err := d.QueryRow(`SELECT id FROM users WHERE role='admin'`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	otherToken, err := db.CreateSession(d, uid, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other := &http.Cookie{Name: sessionCookie, Value: otherToken}
	s, _ := New(d)

	rec := cookieDo(t, s, admin, "POST", "/api/change-password", map[string]any{"old_password": "testpass", "new_password": "testpass-2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("change password: %d %s", rec.Code, rec.Body)
	}
	if rec := cookieDo(t, s, admin, "GET", "/api/nodes", nil); rec.Code != http.StatusOK {
		t.Fatalf("the session that changed the password was ended: %d", rec.Code)
	}
	if rec := cookieDo(t, s, other, "GET", "/api/nodes", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("another session of the same user survived the password change: %d", rec.Code)
	}
}

// The new hash and the removal of the old sessions are one transaction: if the
// sessions cannot be removed, the password must not have changed either.
func TestChangePasswordIsAllOrNothing(t *testing.T) {
	d := openDB(t)
	admin := loginAsAdmin(t, d)
	var uid int64
	var before string
	if err := d.QueryRow(`SELECT id, pw_hash FROM users WHERE role='admin'`).Scan(&uid, &before); err != nil {
		t.Fatal(err)
	}
	// A second session, so that there is a row to remove.
	if _, err := db.CreateSession(d, uid, time.Hour); err != nil {
		t.Fatal(err)
	}
	s, _ := New(d)
	// Reads of sessions keep working (the request has to authenticate);
	// deleting from it does not.
	if _, err := d.Exec(`CREATE TRIGGER sessions_no_delete BEFORE DELETE ON sessions BEGIN SELECT RAISE(ABORT, 'sessions cannot be deleted'); END`); err != nil {
		t.Fatal(err)
	}
	rec := cookieDo(t, s, admin, "POST", "/api/change-password", map[string]any{"old_password": "testpass", "new_password": "testpass-2"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 when the sessions cannot be removed, got %d %s", rec.Code, rec.Body)
	}
	var after string
	if err := d.QueryRow(`SELECT pw_hash FROM users WHERE role='admin'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("the password changed although the old sessions could not be removed")
	}
}

// A user is created together with the settings it was created with. If one of
// them cannot be stored there must be no user: retrying would otherwise hit
// "username exists" and the half-made account could not be completed.
func TestCreateUserIsAllOrNothing(t *testing.T) {
	d := openDB(t)
	admin := loginAsAdmin(t, d)
	s, _ := New(d)
	if _, err := d.Exec(`CREATE TRIGGER users_no_expiry BEFORE UPDATE OF expires_at ON users BEGIN SELECT RAISE(ABORT, 'expiry cannot be stored'); END`); err != nil {
		t.Fatal(err)
	}
	rec := cookieDo(t, s, admin, "POST", "/api/users", map[string]any{"username": "half", "password": "half-pass-1", "expires_at": "2030-01-01"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 when the expiry cannot be stored, got %d %s", rec.Code, rec.Body)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM users WHERE username='half'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("a user was left behind without the expiry it was created with")
	}
}

// Deleting a node asks for confirmation when a composite refers to it. If the
// referrers cannot be looked up the delete must stop — read as "none", the
// lookup failure would skip the confirmation.
func TestDeleteNodeStopsWhenReferrersAreUnknown(t *testing.T) {
	d := openDB(t)
	n := mustNode(t, d, "edge")
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	loseTable(t, d, "node_hops")
	rec := cookieDo(t, s, admin, "DELETE", "/api/nodes/"+strconv.FormatInt(n.ID, 10), nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d %s", rec.Code, rec.Body)
	}
	if _, err := db.GetNode(d, n.ID); err != nil {
		t.Fatalf("the node was deleted without the referrer check: %v", err)
	}
}

// Clearing a node's IPv6 relay address is refused while rules use its v6
// entry. If those rules cannot be counted the address must stay.
func TestClearV6RelayStopsWhenItsRulesAreUnknown(t *testing.T) {
	d := openDB(t)
	n := mustNode(t, d, "edge")
	if err := db.UpdateNodeRelayHostV6(d, n.ID, "2001:db8::1"); err != nil {
		t.Fatal(err)
	}
	admin := loginAsAdmin(t, d)
	s, _ := New(d)

	loseTable(t, d, "rules")
	rec := cookieDo(t, s, admin, "POST", "/api/nodes/"+strconv.FormatInt(n.ID, 10)+"/relay-host-v6", map[string]any{"relay_host_v6": ""})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d %s", rec.Code, rec.Body)
	}
	got, err := db.GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHostV6 != "2001:db8::1" {
		t.Fatalf("the v6 relay address was changed to %q without the rule check", got.RelayHostV6)
	}
}

// An Idempotency-Key promises one rule per key. If the key cannot be looked
// up, creating anyway would break that promise on exactly the retry it is
// there for.
func TestIdempotentCreateStopsWhenTheKeyCannotBeLookedUp(t *testing.T) {
	d := openDB(t)
	uid, tok := v1UserToken(t, d, 10, db.TokenScopeReadWrite)
	grantedNode(t, d, "tokyo", uid, 5)
	s, _ := New(d)
	create := func() int {
		b, _ := json.Marshal(map[string]any{"node_name": "tokyo", "name": "once", "proto": "tcp", "exit": "9.9.9.9:443"})
		req := httptest.NewRequest("POST", "/api/v1/my/rules", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Idempotency-Key", "retry-1")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := create(); code != http.StatusOK {
		t.Fatalf("first create: %d", code)
	}
	loseTable(t, d, "api_idempotency")
	if code := create(); code != http.StatusInternalServerError {
		t.Fatalf("retry with the key unreadable: want 500, got %d", code)
	}
	rules, err := db.ListRulesByUser(d, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("the retry created another rule: %d rules", len(rules))
	}
}

// 下发给节点的规则集由三张查找表拼出（buildRules）。任何一张读不出来，构建都失败：
// 缺着它构建，规则集仍会列出每一跳，但少了那张表供的部分——规则 id 与规则名、跳数、
// 授权设的限速——而节点会照着安装。
func TestBuildRulesFailsWhenALookupTableCannotBeRead(t *testing.T) {
	for _, table := range []string{"rules", "rule_hops", "user_nodes"} {
		d := openDB(t)
		_, n0, _ := seedTwoHopRuleDB(t, d)
		ruleHops, err := db.ActiveRuleHopsForPush(d, n0)
		if err != nil || len(ruleHops) == 0 {
			t.Fatalf("seed: %d hops, err=%v", len(ruleHops), err)
		}
		if _, err := buildRules(d, ruleHops); err != nil {
			t.Fatalf("control build: %v", err)
		}
		loseTable(t, d, table)
		if rules, err := buildRules(d, ruleHops); err == nil {
			t.Errorf("%s unreadable: build succeeded with %d rules", table, len(rules))
		}
	}
}

// The agent applies the pool size a hello ack carries and keeps its own when
// the ack carries none. When the setting cannot be read the ack must carry
// none, or the default would replace the size the agent runs with.
func TestHubHelloAckOmitsPoolSizeWhenItCannotBeRead(t *testing.T) {
	srv, hub, _ := newHubTestServer(t)
	loseTable(t, hub.DB, "settings")
	c := dialWS(t, srv)
	hp, _ := json.Marshal(wsproto.Hello{NodeToken: "tok-good", AgentVersion: "v1.0", OS: "linux", Arch: "amd64"})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hp})
	env := recvEnvelope(t, c)
	if env.Type != wsproto.TypeHelloAck {
		t.Fatalf("want a hello ack, got %s", env.Type)
	}
	if strings.Contains(string(env.Payload), "pool_size") {
		t.Fatalf("hello ack carries a pool size although the setting could not be read: %s", env.Payload)
	}
}

// 组合成员校验凭它读出的拓扑决定一份成员列表能不能保存。拓扑读不出来时必须回答 500：
// 400 在这里的含义是“成员不存在或会成环”，那是在告诉管理员他提交的列表有问题。
//
// 规则写入一侧的同类检查（节点授权、条数上限、授权覆盖、推导链用的拓扑）在
// internal/rulechange 的 TestGuardsReportAFailedLookupInsteadOfPassing。
func TestCompositeMemberCheckReportsAFailedLookup(t *testing.T) {
	for _, table := range []string{"nodes", "node_hops", "node_bindings"} {
		d := openDB(t)
		uid, _ := loginAsUser(t, d, 10)
		node := grantedNode(t, d, "edge", uid, 5)
		composite := makeComposite(t, d, "pair", node.ID)
		s, _ := New(d)
		if status, err := s.validateCompositeMembers(0, []int64{composite.ID}); err != nil {
			t.Fatalf("对照：表都在时应通过，实际 status=%d err=%v", status, err)
		}
		loseTable(t, d, table)
		status, err := s.validateCompositeMembers(0, []int64{composite.ID})
		if err == nil || status != http.StatusInternalServerError {
			t.Errorf("%s 读不出来：status=%d err=%v，应为 500 且带错误", table, status, err)
		}
	}
}
