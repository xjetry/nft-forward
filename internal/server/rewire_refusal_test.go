package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Deleting a node, or changing its relay address, re-generates the rules
// that run over it (db.DeleteNodeAndRewire, db.SetNodeRelayHostAndRewire).
// When one of those rules could not run over the nodes as the change would
// leave them, the change is refused — naming the rule and the reason, so the
// admin knows what to change first — and nothing of it is stored. Applied
// anyway, it would leave the rule's stored hops pointing at a node or an
// address that is gone, with no one told.
func TestChangeThatWouldStrandARuleIsRefused(t *testing.T) {
	seed := seedFaultDB(t)
	rule := fmt.Sprintf("规则 #%d", seed.ruleID)
	for i, c := range []struct {
		name    string
		prepare []string // SQL run first, each with the rule id and the second hop's node id
		method  string
		url     string
		body    string
		refused string
	}{
		{
			// Every hop node needs a relay address for the hop before it to
			// target.
			name:   "clearing the relay address of a node a rule runs over",
			method: http.MethodPost, url: fmt.Sprintf("/api/nodes/%d/relay-host", seed.viaID), body: `{"relay_host":""}`,
			refused: "无法修改中继地址",
		},
		{
			// The rule exits to an IPv6 address, which only its last hop has
			// the IPv6 relay address for; with that node deleted, the hop
			// before it becomes the last and cannot reach the exit.
			name: "deleting the only node of a rule that can reach its IPv6 exit",
			prepare: []string{
				`UPDATE rules SET exit_host='2001:db8::1' WHERE id=?1`,
				`UPDATE nodes SET relay_host_v6='2001:db8::11' WHERE id=?2`,
			},
			method: http.MethodDelete, url: fmt.Sprintf("/api/nodes/%d?confirm=1", seed.viaID),
			refused: "无法删除节点",
		},
	} {
		s, _, release, err := openFaulted(seed, filepath.Join(t.TempDir(), fmt.Sprintf("refusal-%d.db", i)))
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range c.prepare {
			if _, err := s.DB.Exec(stmt, seed.ruleID, seed.viaID); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
		}
		before, err := storedState(s.DB)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(c.method, c.url, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		seed.authenticate(req, "admin")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		after, err := storedState(s.DB)
		if err != nil {
			t.Fatal(err)
		}
		reply := strings.TrimSpace(rec.Body.String())
		if rec.Code != http.StatusConflict || !strings.Contains(reply, c.refused) || !strings.Contains(reply, rule) {
			t.Errorf("%s: %d %s, want 409 saying %q and naming %q", c.name, rec.Code, reply, c.refused, rule)
		}
		if before != after {
			t.Errorf("%s: answered %d, but part of the change is stored", c.name, rec.Code)
		}
		release()
	}
}
