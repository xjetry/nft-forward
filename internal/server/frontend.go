package server

import (
	"encoding/json"
	"html"
	"io/fs"
	"net/http"
	"strings"

	"nft-forward/internal/db"
	"nft-forward/web"
)

// defaultIndexTitle is the built-in <title> baked into web/dist/index.html;
// renderIndex swaps it for the configured panel name.
const defaultIndexTitle = "<title>nft-forward</title>"

func (s *Server) spaHandler() http.Handler {
	dist, err := fs.Sub(web.Assets, "dist")
	if err != nil {
		panic("embedded web/dist not found: " + err.Error())
	}
	files := http.FileServerFS(dist)
	index, _ := fs.ReadFile(dist, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" && p != "index.html" {
			if _, err := fs.Stat(dist, p); err == nil {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(s.renderIndex(index))
	})
}

// renderIndex stamps the configured panel name into the SPA shell. The
// browser paints <title> and the login page before any API response
// arrives, so branding must ride on the HTML itself rather than a
// follow-up /branding fetch; window.__BRANDING__ gives the JS side the
// same value without a flash of the default name. Reads the setting per
// request so renames take effect without a restart.
func (s *Server) renderIndex(index []byte) []byte {
	name, _ := db.GetSetting(s.DB, "panel_name")
	if name == "" {
		return index
	}
	page := strings.Replace(string(index), defaultIndexTitle, "<title>"+html.EscapeString(name)+"</title>", 1)
	// json.Marshal escapes <, > and & so a name containing "</script>"
	// cannot break out of the inline script.
	payload, _ := json.Marshal(map[string]string{"panel_name": name})
	page = strings.Replace(page, "</head>", "<script>window.__BRANDING__ = "+string(payload)+"</script>\n  </head>", 1)
	return []byte(page)
}
