package server

import (
	"io/fs"
	"net/http"
	"strings"

	"nft-forward/web"
)

func spaHandler() http.Handler {
	dist, err := fs.Sub(web.Assets, "dist")
	if err != nil {
		panic("embedded web/dist not found: " + err.Error())
	}
	files := http.FileServerFS(dist)
	index, _ := fs.ReadFile(dist, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(dist, p); err == nil {
			if strings.HasPrefix(p, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	})
}
