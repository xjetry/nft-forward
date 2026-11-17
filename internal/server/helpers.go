package server

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// urlParamInt64 extracts a chi URL parameter as int64.
func urlParamInt64(r *http.Request, name string) (int64, error) {
	v := chi.URLParam(r, name)
	if v == "" {
		return 0, fmt.Errorf("missing URL param %s", name)
	}
	return strconv.ParseInt(v, 10, 64)
}
