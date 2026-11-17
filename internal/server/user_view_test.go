package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"nft-forward/internal/db"
)

// users.expires_at 只有两种值：NULL（永不到期）或一个正的 unix 时刻。到期判定
// （db.User.Expired 与压制条件里的 SQL）只把 NULL 当作永不到期，所以任何一个写入方
// 把“不设到期”写成 0，这个账号就会被当成 1970 年到期。这里把每个写到期时间的入口
// 都走一遍：不设到期落库为 NULL，设了落库为那个时刻；不成立的取值（负数、认不出的
// 日期）被拒绝并且不落库。
func TestEveryWriterStoresNoExpiryAsNull(t *testing.T) {
	d := openDB(t)
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	admin := loginAsAdmin(t, d)
	adminRow, err := db.GetUserByUsername(d, "admin-test")
	if err != nil {
		t.Fatal(err)
	}
	token, err := db.CreateAPIToken(d, adminRow.ID, "readwrite")
	if err != nil {
		t.Fatal(err)
	}

	const day = "2099-01-01"
	dayUnix := strconv.FormatInt(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), 10)
	const unix = int64(4102444800)
	unixText := strconv.FormatInt(unix, 10)

	// stored 是库里的值："NULL"，或那个整数的十进制写法。
	stored := func(id int64) string {
		t.Helper()
		var v sql.NullInt64
		if err := d.QueryRow(`SELECT expires_at FROM users WHERE id=?`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if !v.Valid {
			return "NULL"
		}
		return strconv.FormatInt(v.Int64, 10)
	}
	idOf := func(username string) (int64, bool) {
		t.Helper()
		var id int64
		err := d.QueryRow(`SELECT id FROM users WHERE username=?`, username).Scan(&id)
		if err == sql.ErrNoRows {
			return 0, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return id, true
	}
	page := func(method, path string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		return adminRequest(t, s, admin, method, path, body)
	}
	v1 := func(method, path string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		return v1Do(t, s, method, path, token, body)
	}
	const pw = "pw-12345678-abc"

	for _, c := range []struct {
		name, username string
		do             func() *httptest.ResponseRecorder
		want           string
	}{
		{"页面建用户，不填到期日", "c1", func() *httptest.ResponseRecorder {
			return page("POST", "/api/users", map[string]any{"username": "c1", "password": pw, "role": "user"})
		}, "NULL"},
		{"页面建用户，填到期日", "c2", func() *httptest.ResponseRecorder {
			return page("POST", "/api/users", map[string]any{"username": "c2", "password": pw, "role": "user", "expires_at": day})
		}, dayUnix},
		{"v1 建用户，不带 expires_at", "c3", func() *httptest.ResponseRecorder {
			return v1("POST", "/api/v1/users", map[string]any{"username": "c3"})
		}, "NULL"},
		{"v1 建用户，expires_at 为 0", "c4", func() *httptest.ResponseRecorder {
			return v1("POST", "/api/v1/users", map[string]any{"username": "c4", "expires_at": 0})
		}, "NULL"},
		{"v1 建用户，带到期时刻", "c5", func() *httptest.ResponseRecorder {
			return v1("POST", "/api/v1/users", map[string]any{"username": "c5", "expires_at": unix})
		}, unixText},
	} {
		rec := c.do()
		if rec.Code != http.StatusOK {
			t.Fatalf("%s：%d %s", c.name, rec.Code, rec.Body.String())
		}
		id, ok := idOf(c.username)
		if !ok {
			t.Fatalf("%s：用户没有建出来", c.name)
		}
		if got := stored(id); got != c.want {
			t.Errorf("%s：库里的到期时间是 %s，期望 %s", c.name, got, c.want)
		}
	}

	id, _ := idOf("c1")
	pageExpiry := fmt.Sprintf("/api/users/%d/expiry", id)
	pageProfile := fmt.Sprintf("/api/users/%d/profile", id)
	v1Expiry := fmt.Sprintf("/api/v1/users/%d/expiry", id)
	for _, c := range []struct {
		name string
		rec  func() *httptest.ResponseRecorder
		want string
	}{
		{"页面设到期日", func() *httptest.ResponseRecorder { return page("POST", pageExpiry, map[string]any{"expires_at": day}) }, dayUnix},
		{"页面清掉到期日", func() *httptest.ResponseRecorder { return page("POST", pageExpiry, map[string]any{"expires_at": ""}) }, "NULL"},
		{"v1 设到期时刻", func() *httptest.ResponseRecorder { return v1("PUT", v1Expiry, map[string]any{"expires_at": unix}) }, unixText},
		{"v1 清掉到期时刻（0）", func() *httptest.ResponseRecorder { return v1("PUT", v1Expiry, map[string]any{"expires_at": 0}) }, "NULL"},
		{"页面改资料，填到期日", func() *httptest.ResponseRecorder {
			return page("PATCH", pageProfile, map[string]any{"expires_at": day, "max_forwards": 5})
		}, dayUnix},
		{"页面改资料，不填到期日", func() *httptest.ResponseRecorder {
			return page("PATCH", pageProfile, map[string]any{"expires_at": "", "max_forwards": 5})
		}, "NULL"},
	} {
		if rec := c.rec(); rec.Code != http.StatusOK {
			t.Fatalf("%s：%d %s", c.name, rec.Code, rec.Body.String())
		}
		if got := stored(id); got != c.want {
			t.Errorf("%s：库里的到期时间是 %s，期望 %s", c.name, got, c.want)
		}
	}

	// 不成立的取值：拒绝，不落库。
	if rec := v1("PUT", v1Expiry, map[string]any{"expires_at": unix}); rec.Code != http.StatusOK {
		t.Fatalf("前置：%d %s", rec.Code, rec.Body.String())
	}
	if rec := v1("PUT", v1Expiry, map[string]any{"expires_at": -1}); rec.Code != http.StatusBadRequest {
		t.Errorf("v1 设负的到期时刻：回了 %d %s，应回 400", rec.Code, rec.Body.String())
	}
	if got := stored(id); got != unixText {
		t.Errorf("被拒绝的请求不应改动到期时间：现在是 %s，原来是 %s", got, unixText)
	}
	if rec := v1("POST", "/api/v1/users", map[string]any{"username": "bad1", "expires_at": -1}); rec.Code != http.StatusBadRequest {
		t.Errorf("v1 建用户带负的到期时刻：回了 %d %s，应回 400", rec.Code, rec.Body.String())
	}
	if _, ok := idOf("bad1"); ok {
		t.Error("v1 建用户被拒绝后不应留下用户")
	}
	if rec := page("POST", "/api/users", map[string]any{"username": "bad2", "password": pw, "role": "user", "expires_at": "not-a-date"}); rec.Code != http.StatusBadRequest {
		t.Errorf("页面建用户带认不出的到期日：回了 %d %s，应回 400", rec.Code, rec.Body.String())
	}
	if _, ok := idOf("bad2"); ok {
		t.Error("页面建用户被拒绝后不应留下用户")
	}
	// 1970-01-01 换成 unix 秒正好是 0，更早的是负数：都不是到期时刻，也不是“不设到期”。
	for _, epoch := range []string{"1970-01-01", "1969-12-31"} {
		if rec := page("POST", pageExpiry, map[string]any{"expires_at": epoch}); rec.Code != http.StatusBadRequest {
			t.Errorf("页面把到期日设成 %s：回了 %d %s，应回 400", epoch, rec.Code, rec.Body.String())
		}
		if rec := page("PATCH", pageProfile, map[string]any{"expires_at": epoch, "max_forwards": 5}); rec.Code != http.StatusBadRequest {
			t.Errorf("页面改资料把到期日设成 %s：回了 %d %s，应回 400", epoch, rec.Code, rec.Body.String())
		}
		if got := stored(id); got != unixText {
			t.Errorf("到期日 %s 被拒绝后不应改动到期时间：现在是 %s，原来是 %s", epoch, got, unixText)
		}
	}
}

// 管理员看到的用户只有一个形状：用户列表里的每一项、用户详情里的 user、建用户的回答
// 里的 user，键与值都相同；列表另带一个只有它需要的 rule_count。到期时间是一个数或
// null，禁用原因是一个字符串或 null——不是数据库驱动的 {"Int64":…,"Valid":…}。
func TestAdminSeesOneUserShape(t *testing.T) {
	d := openDB(t)
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	admin := loginAsAdmin(t, d)
	const day = "2099-01-01"
	dayUnix := float64(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC).Unix())

	get := func(path string, out any) {
		t.Helper()
		rec := adminRequest(t, s, admin, "GET", path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("GET %s: %v（%s）", path, err, rec.Body.String())
		}
	}
	keys := func(m map[string]any) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}

	created := map[string]map[string]any{}
	for name, body := range map[string]map[string]any{
		"never": {"username": "never", "password": "pw-12345678-abc", "role": "user", "admin_note": "记一笔"},
		"dated": {"username": "dated", "password": "pw-12345678-abc", "role": "user", "expires_at": day},
	} {
		rec := adminRequest(t, s, admin, "POST", "/api/users", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("建用户 %s：%d %s", name, rec.Code, rec.Body.String())
		}
		var reply struct {
			User map[string]any `json:"user"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || reply.User == nil {
			t.Fatalf("建用户 %s 的回答：%v（%s）", name, err, rec.Body.String())
		}
		created[name] = reply.User
	}

	var list struct {
		Users []map[string]any `json:"users"`
	}
	get("/api/users", &list)
	if len(list.Users) != 3 {
		t.Fatalf("列表里应有 3 个用户（管理员与刚建的两个），实际 %d", len(list.Users))
	}
	for _, item := range list.Users {
		name, _ := item["username"].(string)
		id, _ := item["id"].(float64)
		if _, ok := item["rule_count"].(float64); !ok {
			t.Errorf("%s：列表项的 rule_count 应是一个数，实际 %#v", name, item["rule_count"])
		}
		var detail struct {
			User map[string]any `json:"user"`
		}
		get("/api/users/"+strconv.FormatInt(int64(id), 10), &detail)

		listed := map[string]any{}
		for k, v := range item {
			if k != "rule_count" {
				listed[k] = v
			}
		}
		if !reflect.DeepEqual(listed, detail.User) {
			t.Errorf("%s：列表项与详情里的 user 不是同一个形状\n列表：%v %v\n详情：%v %v",
				name, keys(listed), listed, keys(detail.User), detail.User)
		}
		if c, ok := created[name]; ok && !reflect.DeepEqual(c, detail.User) {
			t.Errorf("%s：建用户的回答与详情里的 user 不是同一个形状\n回答：%v %v\n详情：%v %v",
				name, keys(c), c, keys(detail.User), detail.User)
		}

		switch name {
		case "dated":
			if item["expires_at"] != dayUnix {
				t.Errorf("dated：expires_at 应是 %v，实际 %#v", dayUnix, item["expires_at"])
			}
		default:
			if v, present := item["expires_at"]; !present || v != nil {
				t.Errorf("%s：没有到期时间，expires_at 应是 null，实际 %#v（有这个键：%v）", name, v, present)
			}
		}
		if v, present := item["disable_reason"]; !present || v != nil {
			t.Errorf("%s：没被禁用，disable_reason 应是 null，实际 %#v（有这个键：%v）", name, v, present)
		}
		if name == "never" && item["admin_note"] != "记一笔" {
			t.Errorf("never：admin_note 应是 %q，实际 %#v", "记一笔", item["admin_note"])
		}
	}
}

// 管理备注是管理员写给自己看的（页面上的说明就是“只有管理员能看到”），不出现在用户
// 本人能读到的任何回答里：会话下的 /api/me、/api/my 与其余几个只读接口，以及他自己
// 的 token 能读的 /api/v1 接口。对照组：管理员看这个用户时带着它。
func TestAdminNoteIsNotShownToTheUser(t *testing.T) {
	d := openDB(t)
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	admin := loginAsAdmin(t, d)
	uid, cookie := loginAsUser(t, d, 10)
	const note = "note-only-for-the-admin-7f3a"
	if _, err := d.Exec(`UPDATE users SET admin_note=? WHERE id=?`, note, uid); err != nil {
		t.Fatal(err)
	}
	token, err := db.CreateAPIToken(d, uid, "read")
	if err != nil {
		t.Fatal(err)
	}
	n, err := db.CreateNode(d, "n", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateNodeRelayHost(d, n.ID, "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := db.GrantNode(d, uid, n.ID, 10, 0); err != nil {
		t.Fatal(err)
	}
	rec := userJSON(t, s, cookie, "POST", "/api/my/rules", map[string]any{
		"node_id": n.ID, "name": "r", "proto": "tcp", "exit": "9.9.9.9:443",
	})
	var made struct {
		RuleID int64 `json:"rule_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &made); err != nil || made.RuleID == 0 {
		t.Fatalf("建规则的回答：%v（%s）", err, rec.Body.String())
	}
	rule := strconv.FormatInt(made.RuleID, 10)

	type call struct{ path, token string }
	calls := []call{
		{"/api/me", ""}, {"/api/my", ""}, {"/api/my/rules", ""}, {"/api/my/rules/" + rule, ""},
		{"/api/my/landing-nodes", ""}, {"/api/my/token", ""},
		{"/api/v1/info", token},
		{"/api/v1/my/nodes", token}, {"/api/v1/my/rules", token}, {"/api/v1/my/rules/" + rule, token},
	}
	for _, c := range calls {
		req := httptest.NewRequest("GET", c.path, nil)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		} else {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s：%d %s（这条要验证的是回答里没有备注，先得读得到）", c.path, rec.Code, rec.Body.String())
			continue
		}
		body := rec.Body.String()
		if strings.Contains(body, note) || strings.Contains(body, "admin_note") {
			t.Errorf("GET %s 把管理备注发给了用户本人：%s", c.path, body)
		}
	}

	rec = adminRequest(t, s, admin, "GET", "/api/users/"+strconv.FormatInt(uid, 10), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), note) {
		t.Errorf("管理员看这个用户时应带着备注：%d %s", rec.Code, rec.Body.String())
	}
}
