package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nft-forward/internal/db"
)

// 没有这个接口的请求回 404，不回前端的入口页。
//
// 路由表里没有的路径由根路由的 NotFound 接住，它回的是前端入口页：前端自己的路由
// （/nodes、/users/3 这些）在刷新或直接打开时要靠它才回得到页面。/api 与 /api/v1 之下
// 没有页面：一个写错的、或已经删掉的接口地址若被答成 200 加一页 HTML，调用方读到的
// 就是“成功”。
//
// 对照组钉住反方向：不在 /api 之下的未知路径仍然回入口页，已有的接口照常回答。
func TestUnknownAPIPathIsAnswered404NotThePage(t *testing.T) {
	d := openDB(t)
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	cookie := loginAsAdmin(t, d)
	var adminID int64
	if err := d.QueryRow(`SELECT id FROM users WHERE role='admin'`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	token, err := db.CreateAPIToken(d, adminID, "readwrite")
	if err != nil {
		t.Fatal(err)
	}

	do := func(method, path string, signedIn bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		if signedIn {
			if strings.HasPrefix(path, "/api/v1/") {
				req.Header.Set("Authorization", "Bearer "+token)
			} else {
				req.AddCookie(cookie)
			}
		}
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		return rec
	}

	for _, path := range []string{
		"/api/no-such-route",
		"/api/users/1/no-such-action",
		"/api/v1/no-such-route",
		"/api/v1/nodes/1/no-such-action",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			for _, signedIn := range []bool{false, true} {
				rec := do(method, path, signedIn)
				where := method + " " + path
				if signedIn {
					where += "（已登录）"
				}
				if rec.Code != http.StatusNotFound {
					t.Errorf("%s: 回了 %d %.40q，应回 404", where, rec.Code, rec.Body.String())
					continue
				}
				if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
					t.Errorf("%s: Content-Type 是 %q，应是 JSON", where, ct)
				}
				if strings.HasPrefix(path, "/api/v1/") {
					var reply struct {
						Error struct{ Code, Message string }
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || reply.Error.Code != codeNotFound || reply.Error.Message == "" {
						t.Errorf("%s: 回答是 %s，应是 /api/v1 的错误信封、code=%s", where, rec.Body.String(), codeNotFound)
					}
				} else {
					var reply struct{ Error string }
					if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil || reply.Error == "" {
						t.Errorf("%s: 回答是 %s，应是 {\"error\": …}", where, rec.Body.String())
					}
				}
			}
		}
	}

	// 对照组。
	if rec := do(http.MethodGet, "/no-such-page", false); rec.Code != http.StatusOK ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
		t.Errorf("GET /no-such-page: 回了 %d %q，前端的路径应当回入口页", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := do(http.MethodGet, "/api/me", true); rec.Code != http.StatusOK {
		t.Errorf("GET /api/me（已登录）: 回了 %d，已有的接口应照常回答", rec.Code)
	}
	if rec := do(http.MethodGet, "/api/v1/info", true); rec.Code != http.StatusOK {
		t.Errorf("GET /api/v1/info（带令牌）: 回了 %d，已有的接口应照常回答", rec.Code)
	}
}
