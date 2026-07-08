package server

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var openapiJSON []byte

// serveOpenAPI returns the hand-written OpenAPI document. It's the public
// contract, so it needs no token — mounted outside requireTokenAuth.
func (s *Server) serveOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(openapiJSON)
}
