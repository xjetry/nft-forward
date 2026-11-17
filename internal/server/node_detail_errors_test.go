package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"nft-forward/internal/db"
)

// The node detail endpoint must not present a failed list query as an empty
// list: the page reads "no rules", "no granted users" and "no members" as
// facts about the node. Each case removes the table one of those queries
// reads, so that query — and only a query on that table — fails.
func TestAPIGetNodeFailsWhenAListQueryFails(t *testing.T) {
	for _, tc := range []struct {
		name      string
		composite bool
		dropTable string
		want      int
	}{
		{name: "nothing broken", want: http.StatusOK},
		{name: "nothing broken, composite", composite: true, want: http.StatusOK},
		{name: "rule list", dropTable: "rule_hops", want: http.StatusInternalServerError},
		{name: "granted users", dropTable: "user_nodes", want: http.StatusInternalServerError},
		{name: "composite members", composite: true, dropTable: "node_hops", want: http.StatusInternalServerError},
	} {
		d := openDB(t)
		n, err := db.CreateNode(d, "detail-node", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if tc.composite {
			if _, err := d.Exec(`UPDATE nodes SET node_type='composite' WHERE id=?`, n.ID); err != nil {
				t.Fatal(err)
			}
		}
		s, _ := New(d)
		admin := loginAsAdmin(t, d)
		if tc.dropTable != "" {
			if _, err := d.Exec(`DROP TABLE ` + tc.dropTable); err != nil {
				t.Fatalf("%s: drop %s: %v", tc.name, tc.dropTable, err)
			}
		}
		req := httptest.NewRequest("GET", fmt.Sprintf("/api/nodes/%d", n.ID), nil)
		req.AddCookie(admin)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("%s: want %d, got %d: %.200s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}
}
