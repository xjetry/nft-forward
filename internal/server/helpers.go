package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
)

// flashRedirect sets a flash message and issues a 303 redirect. Replaces the
// repeated setFlash + http.Redirect + return triple that appeared 35+ times.
func (s *Server) flashRedirect(w http.ResponseWriter, r *http.Request, msg, path string) {
	setFlash(w, msg)
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// parseFormInt parses an integer form value, returning a descriptive error
// when the field is empty or not a valid integer.
func parseFormInt(r *http.Request, field string) (int, error) {
	v := strings.TrimSpace(r.FormValue(field))
	if v == "" {
		return 0, fmt.Errorf("字段 %s 不能为空", field)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("字段 %s 不是有效整数", field)
	}
	return n, nil
}

// parseFormInt64 parses an int64 form value.
func parseFormInt64(r *http.Request, field string) (int64, error) {
	v := strings.TrimSpace(r.FormValue(field))
	if v == "" {
		return 0, fmt.Errorf("字段 %s 不能为空", field)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("字段 %s 不是有效整数", field)
	}
	return n, nil
}

// urlParamInt64 extracts a chi URL parameter as int64.
func urlParamInt64(r *http.Request, name string) (int64, error) {
	v := chi.URLParam(r, name)
	if v == "" {
		return 0, fmt.Errorf("missing URL param %s", name)
	}
	return strconv.ParseInt(v, 10, 64)
}

// validMode checks that a forwarding mode is one of the known values.
func validMode(mode string) bool {
	return mode == "" || mode == "kernel" || mode == "userspace"
}

// buildMap creates a lookup map from a slice, keyed by the result of the key
// function. Replaces the repeated for-range map-building loops.
func buildMap[T any](items []*T, key func(*T) int64) map[int64]*T {
	m := make(map[int64]*T, len(items))
	for _, item := range items {
		m[key(item)] = item
	}
	return m
}

// PageInfo holds pagination state passed to templates.
type PageInfo struct {
	Page       int
	TotalPages int
	Total      int
}

const perPage = 10

// paginate slices items for the requested page and returns the page slice
// plus PageInfo for the template pager.
func paginate[T any](items []*T, r *http.Request) ([]*T, PageInfo) {
	total := len(items)
	totalPages := (total + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * perPage
	end := start + perPage
	if end > total {
		end = total
	}
	return items[start:end], PageInfo{Page: page, TotalPages: totalPages, Total: total}
}
