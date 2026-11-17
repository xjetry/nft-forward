package db

import (
	"testing"

	"nft-forward/internal/chain"
)

// A new grant inherits (roles = 0) so existing behavior is unchanged.
func TestGrantSeatsDefaultInherit(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "gr")
	grantNode(t, d, uid, nid)

	g, err := GetNodeGrant(d, uid, nid)
	if err != nil {
		t.Fatal(err)
	}
	if g.Seats != 0 {
		t.Fatalf("new grant roles = %d, want 0 (inherit)", g.Seats)
	}
}

// Every user_nodes read path must return the same override — one misaligned
// scan would silently shift the grant columns.
func TestGrantSeatsReadAlignment(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "gr")
	grantNode(t, d, uid, nid)
	if _, err := d.Exec(`UPDATE user_nodes SET seats=1 WHERE user_id=? AND node_id=?`, uid, nid); err != nil {
		t.Fatal(err)
	}

	g, err := GetNodeGrant(d, uid, nid)
	if err != nil {
		t.Fatal(err)
	}
	if g.Seats != 1 {
		t.Fatalf("GetNodeGrant roles = %d, want 1", g.Seats)
	}

	_, grants, err := ListNodesForUser(d, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].Seats != 1 {
		t.Fatalf("ListNodesForUser roles = %v, want [1]", grants)
	}

	users, err := ListUsersForNode(d, nid)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Seats != 1 {
		t.Fatalf("ListUsersForNode roles = %v, want 1", users)
	}
}

func TestEffectiveSeats(t *testing.T) {
	// grant 0 inherits the node mask
	if got := EffectiveSeats(chain.SeatVia, 0); got != chain.SeatVia {
		t.Fatalf("inherit = %d, want %d", got, chain.SeatVia)
	}
	// override may add a bit the node lacks (via node opened as entry)
	if got := EffectiveSeats(chain.SeatVia, chain.SeatEntry); got != chain.SeatEntry {
		t.Fatalf("override-add = %d, want %d", got, chain.SeatEntry)
	}
	// override may drop a bit the node has (entry+via node narrowed to via)
	if got := EffectiveSeats(chain.SeatEntry|chain.SeatVia, chain.SeatVia); got != chain.SeatVia {
		t.Fatalf("override-narrow = %d, want %d", got, chain.SeatVia)
	}
}

func TestSetGrantSeats(t *testing.T) {
	d := openTestDB(t)
	uid := createTestUser(t, d)
	nid := createTestNode(t, d, "gr")
	grantNode(t, d, uid, nid)

	if err := SetGrantSeats(d, uid, nid, chain.SeatEntry); err != nil {
		t.Fatal(err)
	}
	g, err := GetNodeGrant(d, uid, nid)
	if err != nil {
		t.Fatal(err)
	}
	if g.Seats != chain.SeatEntry {
		t.Fatalf("roles = %d, want %d", g.Seats, chain.SeatEntry)
	}
}
