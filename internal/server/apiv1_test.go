package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nft-forward/internal/db"
)

// --- helpers ---

func v1UserToken(t *testing.T, d *sql.DB, maxForwards int, scope string) (int64, string) {
	t.Helper()
	uid, _ := loginAsUser(t, d, maxForwards)
	tok, err := db.CreateAPIToken(d, uid, scope)
	if err != nil {
		t.Fatal(err)
	}
	return uid, tok
}

func v1AdminToken(t *testing.T, d *sql.DB, scope string) (int64, string) {
	t.Helper()
	hash, _ := HashPassword("pw")
	uid, err := db.CreateUser(d, fmt.Sprintf("admin-%d", time.Now().UnixNano()), hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := db.CreateAPIToken(d, uid, scope)
	if err != nil {
		t.Fatal(err)
	}
	return uid, tok
}

func v1Do(t *testing.T, s *Server, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

// v1Data decodes the {"data":...} envelope and returns the data as a map.
func v1Data(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode data envelope: %v (body=%s)", err, rec.Body.String())
	}
	return env.Data
}

// v1ErrCode decodes the {"error":{"code","message"}} envelope and returns code.
func v1ErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, rec.Body.String())
	}
	return env.Error.Code
}

func grantedNode(t *testing.T, d *sql.DB, name string, uid int64, maxForwards int) *db.Node {
	t.Helper()
	n, err := db.CreateNode(d, name, "https://p", "t-"+name)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateNodeRelayHost(d, n.ID, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantNode(d, uid, n.ID, maxForwards, 0); err != nil {
		t.Fatal(err)
	}
	return n
}

// --- token auth ---

func TestV1TokenAuthRejections(t *testing.T) {
	d := openDB(t)
	uid, tok := v1UserToken(t, d, 10, db.TokenScopeRead)
	s, _ := New(d)

	// missing token
	if rec := v1Do(t, s, "GET", "/api/v1/info", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: want 401, got %d", rec.Code)
	} else if code := v1ErrCode(t, rec); code != codeUnauthorized {
		t.Fatalf("missing token: want code %q, got %q", codeUnauthorized, code)
	}
	// invalid token
	if rec := v1Do(t, s, "GET", "/api/v1/info", "deadbeef", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token: want 401, got %d", rec.Code)
	}
	// disabled token
	if _, err := db.ToggleAPIToken(d, uid); err != nil {
		t.Fatal(err)
	}
	if rec := v1Do(t, s, "GET", "/api/v1/info", tok, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("disabled token: want 403, got %d", rec.Code)
	}
	// re-enable, then disable the account
	if _, err := db.ToggleAPIToken(d, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE users SET disabled=1 WHERE id=?`, uid); err != nil {
		t.Fatal(err)
	}
	if rec := v1Do(t, s, "GET", "/api/v1/info", tok, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("disabled account: want 403, got %d", rec.Code)
	}
}

func TestV1InfoEnvelopeAndScope(t *testing.T) {
	d := openDB(t)
	_, tok := v1UserToken(t, d, 10, db.TokenScopeReadWrite)
	s, _ := New(d)

	rec := v1Do(t, s, "GET", "/api/v1/info", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("info: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	data := v1Data(t, rec)
	if data["scope"] != db.TokenScopeReadWrite {
		t.Fatalf("info scope: want %q, got %v", db.TokenScopeReadWrite, data["scope"])
	}
	if data["role"] != "user" {
		t.Fatalf("info role: want user, got %v", data["role"])
	}
}

// --- scope gate ---

func TestV1ScopeGateBlocksReadTokenWrite(t *testing.T) {
	d := openDB(t)
	uid, tok := v1UserToken(t, d, 10, db.TokenScopeRead)
	n := grantedNode(t, d, "n1", uid, 5)
	s, _ := New(d)

	// read token may list
	if rec := v1Do(t, s, "GET", "/api/v1/my/rules", tok, nil); rec.Code != http.StatusOK {
		t.Fatalf("read token GET my/rules: want 200, got %d", rec.Code)
	}
	// read token may not create
	rec := v1Do(t, s, "POST", "/api/v1/my/rules", tok, map[string]any{
		"node_id": n.ID, "name": "r", "proto": "tcp", "exit": "9.9.9.9:443",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read token POST: want 403, got %d body=%s", rec.Code, rec.Body.String())
	}
	if code := v1ErrCode(t, rec); code != codeScopeRequired {
		t.Fatalf("read token POST: want code %q, got %q", codeScopeRequired, code)
	}
	if rules, _ := db.ListRulesByUser(d, uid); len(rules) != 0 {
		t.Fatalf("read token must not have created a rule; got %d", len(rules))
	}
}

func TestV1RoleGateBlocksUserFromAdmin(t *testing.T) {
	d := openDB(t)
	_, userTok := v1UserToken(t, d, 10, db.TokenScopeReadWrite)
	_, adminTok := v1AdminToken(t, d, db.TokenScopeRead)
	s, _ := New(d)

	if rec := v1Do(t, s, "GET", "/api/v1/nodes", userTok, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("user token GET /nodes: want 403, got %d", rec.Code)
	}
	if rec := v1Do(t, s, "GET", "/api/v1/nodes", adminTok, nil); rec.Code != http.StatusOK {
		t.Fatalf("admin token GET /nodes: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// --- rule create via v1 ---

func TestV1CreateRuleByName(t *testing.T) {
	d := openDB(t)
	uid, tok := v1UserToken(t, d, 10, db.TokenScopeReadWrite)
	grantedNode(t, d, "tokyo", uid, 5)
	s, _ := New(d)

	rec := v1Do(t, s, "POST", "/api/v1/my/rules", tok, map[string]any{
		"node_name": "tokyo", "name": "byname", "proto": "tcp", "exit": "9.9.9.9:443",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create by name: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	rules, _ := db.ListRulesByUser(d, uid)
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
	// An unknown node name is reported like an ungranted node (403 forbidden),
	// not as a distinct "doesn't exist" — otherwise the name resolution would be
	// an enumeration oracle for nodes the token was never granted.
	rec = v1Do(t, s, "POST", "/api/v1/my/rules", tok, map[string]any{
		"node_name": "nope", "name": "x", "proto": "tcp", "exit": "9.9.9.9:443",
	})
	if rec.Code != http.StatusForbidden || v1ErrCode(t, rec) != codeForbidden {
		t.Fatalf("unknown node name: want 403 forbidden, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestV1CreateRuleNameNoEnumerationOracle pins the invariant that a token cannot
// tell a nonexistent node name apart from a real-but-ungranted one: both must
// yield the same status + error code, so name resolution can't be used to
// enumerate nodes the user was never authorized to see.
func TestV1CreateRuleNameNoEnumerationOracle(t *testing.T) {
	d := openDB(t)
	uid, tok := v1UserToken(t, d, 10, db.TokenScopeReadWrite)
	// A real node the user is NOT granted.
	if _, err := db.CreateNode(d, "secret-node", "https://p", "t-secret"); err != nil {
		t.Fatal(err)
	}
	_ = uid
	s, _ := New(d)

	probe := func(name string) (int, string) {
		rec := v1Do(t, s, "POST", "/api/v1/my/rules", tok, map[string]any{
			"node_name": name, "name": "x", "proto": "tcp", "exit": "9.9.9.9:443",
		})
		return rec.Code, v1ErrCode(t, rec)
	}
	realCode, realErr := probe("secret-node")  // exists, not granted
	fakeCode, fakeErr := probe("no-such-node") // does not exist
	if realCode != fakeCode || realErr != fakeErr {
		t.Fatalf("enumeration oracle: ungranted=%d/%s vs nonexistent=%d/%s must match", realCode, realErr, fakeCode, fakeErr)
	}
	if realCode != http.StatusForbidden {
		t.Fatalf("both cases should be 403, got %d", realCode)
	}
}

func TestV1CreateRuleDryRunDoesNotPersist(t *testing.T) {
	d := openDB(t)
	uid, tok := v1UserToken(t, d, 10, db.TokenScopeReadWrite)
	n := grantedNode(t, d, "dry", uid, 5)
	s, _ := New(d)

	rec := v1Do(t, s, "POST", "/api/v1/my/rules?dry_run=1", tok, map[string]any{
		"node_id": n.ID, "name": "preview", "proto": "tcp", "exit": "9.9.9.9:443",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("dry run: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	data := v1Data(t, rec)
	if data["dry_run"] != true {
		t.Fatalf("dry run: want dry_run=true, got %v", data["dry_run"])
	}
	if data["entry"] == nil || data["entry"] == "" {
		t.Fatalf("dry run should preview an allocated entry; got %v", data["entry"])
	}
	if rules, _ := db.ListRulesByUser(d, uid); len(rules) != 0 {
		t.Fatalf("dry run must not persist a rule; got %d", len(rules))
	}
}

func TestV1CreateRuleIdempotencyReplay(t *testing.T) {
	d := openDB(t)
	uid, tok := v1UserToken(t, d, 10, db.TokenScopeReadWrite)
	n := grantedNode(t, d, "idem", uid, 5)
	s, _ := New(d)

	payload := map[string]any{"node_id": n.ID, "name": "once", "proto": "tcp", "exit": "9.9.9.9:443"}
	mk := func() *httptest.ResponseRecorder {
		b, _ := json.Marshal(payload)
		req := httptest.NewRequest("POST", "/api/v1/my/rules", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Idempotency-Key", "abc-123")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec
	}
	rec1 := mk()
	if rec1.Code != http.StatusOK {
		t.Fatalf("first create: want 200, got %d body=%s", rec1.Code, rec1.Body.String())
	}
	rec2 := mk()
	if rec2.Code != http.StatusOK {
		t.Fatalf("replay: want 200, got %d body=%s", rec2.Code, rec2.Body.String())
	}
	if data := v1Data(t, rec2); data["idempotent_replay"] != true {
		t.Fatalf("second call should be an idempotent replay; got %v", data["idempotent_replay"])
	}
	if rules, _ := db.ListRulesByUser(d, uid); len(rules) != 1 {
		t.Fatalf("idempotent retry must yield exactly 1 rule, got %d", len(rules))
	}
}

func TestV1ListAndGetMyRules(t *testing.T) {
	d := openDB(t)
	uid, tok := v1UserToken(t, d, 10, db.TokenScopeReadWrite)
	n := grantedNode(t, d, "list", uid, 5)
	s, _ := New(d)

	v1Do(t, s, "POST", "/api/v1/my/rules", tok, map[string]any{
		"node_id": n.ID, "name": "r1", "proto": "tcp", "exit": "9.9.9.9:443",
	})
	rules, _ := db.ListRulesByUser(d, uid)
	if len(rules) != 1 {
		t.Fatalf("setup: want 1 rule, got %d", len(rules))
	}
	rid := rules[0].ID

	// list returns an array under data
	rec := v1Do(t, s, "GET", "/api/v1/my/rules", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", rec.Code)
	}
	var listEnv struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listEnv); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listEnv.Data) != 1 {
		t.Fatalf("list: want 1 rule, got %d", len(listEnv.Data))
	}

	// get one
	rec = v1Do(t, s, "GET", fmt.Sprintf("/api/v1/my/rules/%d", rid), tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: want 200, got %d", rec.Code)
	}
	if data := v1Data(t, rec); data["name"] != "r1" {
		t.Fatalf("get: want name r1, got %v", data["name"])
	}

	// another user's token cannot read this rule
	_, otherTok := v1UserToken(t, d, 10, db.TokenScopeReadWrite)
	rec = v1Do(t, s, "GET", fmt.Sprintf("/api/v1/my/rules/%d", rid), otherTok, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user get: want 404, got %d", rec.Code)
	}
}

func TestV1AdminReads(t *testing.T) {
	d := openDB(t)
	_, adminTok := v1AdminToken(t, d, db.TokenScopeRead)
	uid, _ := v1UserToken(t, d, 10, db.TokenScopeRead)
	grantedNode(t, d, "seen", uid, 5)
	s, _ := New(d)

	for _, path := range []string{"/api/v1/nodes", "/api/v1/users", "/api/v1/dashboard", "/api/v1/landing-usage"} {
		rec := v1Do(t, s, "GET", path, adminTok, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	// dashboard aggregates
	rec := v1Do(t, s, "GET", "/api/v1/dashboard", adminTok, nil)
	data := v1Data(t, rec)
	if _, ok := data["nodes_total"]; !ok {
		t.Fatalf("dashboard missing nodes_total: %s", rec.Body.String())
	}
}

// --- token limiter unit tests ---

func TestTokenLimiterRate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newTokenLimiter()
	l.now = func() time.Time { return now }

	for i := 0; i < tokenRateBurst; i++ {
		if !l.allow(1) {
			t.Fatalf("request %d within burst should be allowed", i+1)
		}
	}
	if l.allow(1) {
		t.Fatal("request past burst should be denied")
	}
	// a different token is unaffected
	if !l.allow(2) {
		t.Fatal("unrelated token should be allowed")
	}
	// window rolls over
	now = now.Add(tokenRateWindow)
	if !l.allow(1) {
		t.Fatal("after window reset the token should be allowed again")
	}
}

func TestTokenLimiterShouldTouch(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newTokenLimiter()
	l.now = func() time.Time { return now }

	if !l.shouldTouch(1) {
		t.Fatal("first touch should write")
	}
	if l.shouldTouch(1) {
		t.Fatal("immediate re-touch should be throttled")
	}
	now = now.Add(lastUsedInterval)
	if !l.shouldTouch(1) {
		t.Fatal("after the interval a touch should write again")
	}
}
