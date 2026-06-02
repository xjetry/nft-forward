package db

import (
	"database/sql"
	"testing"
)

func TestOccupiedPortsUnionsForwardsAndTuiSnapshot(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "n1", "https://p", "t")
	// panel segment: one tcp forward holds 20000
	if _, err := CreateForward(d, &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 20000, TargetIP: "1.1.1.1", TargetPort: 1}); err != nil {
		t.Fatal(err)
	}
	// node-local tui segment snapshot: tcp holds 20001, udp holds 53
	if err := UpsertTuiSnapshot(d, n.ID, `[{"proto":"tcp","listen_port":20001,"target_ip":"2.2.2.2","target_port":2},{"proto":"udp","listen_port":53,"target_ip":"3.3.3.3","target_port":3}]`); err != nil {
		t.Fatal(err)
	}
	occ, err := OccupiedPortsOnNode(d, n.ID, "tcp", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !occ[20000] || !occ[20001] {
		t.Fatalf("tcp occupancy should include panel(20000) ∪ tui(20001): %v", occ)
	}
	if occ[53] {
		t.Fatalf("udp port 53 must not appear in tcp occupancy: %v", occ)
	}
}

func TestOccupiedPortsExcludesGivenChain(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "n1", "https://p", "t")
	// Seed a chains row directly so the chain-tagged forward's chain_id FK resolves.
	res, err := d.Exec(`INSERT INTO chains(name,proto,exit_host,exit_port,created_at) VALUES ('c','tcp','9.9.9.9',8443,0)`)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := res.LastInsertId()
	f := &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 20000, TargetIP: "1.1.1.1", TargetPort: 1}
	f.ChainID = sql.NullInt64{Int64: cid, Valid: true}
	if _, err := CreateForward(d, f); err != nil {
		t.Fatal(err)
	}
	// Excluding the chain: the chain's own port must not appear.
	occ, _ := OccupiedPortsOnNode(d, n.ID, "tcp", cid)
	if occ[20000] {
		t.Fatalf("excludeChainID should drop the chain's own port: %v", occ)
	}
	// Without exclusion: port must be visible.
	occ2, _ := OccupiedPortsOnNode(d, n.ID, "tcp", 0)
	if !occ2[20000] {
		t.Fatalf("without exclude the port must be occupied: %v", occ2)
	}
}

func TestOccupiedPortsNoSnapshot(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "n1", "https://p", "t")
	// A node with no forwards and no tui snapshot occupies nothing; the missing
	// node_tui_snapshot row must yield an empty map, not an error.
	occ, err := OccupiedPortsOnNode(d, n.ID, "tcp", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(occ) != 0 {
		t.Fatalf("fresh node should occupy no ports: %v", occ)
	}
}

func TestRelayHostRoundTrip(t *testing.T) {
	d := openMemDB(t)
	n, err := CreateNode(d, "gomami", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if n.RelayHost != "" {
		t.Fatalf("new node relay_host should default empty, got %q", n.RelayHost)
	}
	if err := UpdateNodeRelayHost(d, n.ID, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	got, err := GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHost != "1.2.3.4" {
		t.Fatalf("relay_host = %q, want 1.2.3.4", got.RelayHost)
	}
}

func TestCreateForwardCarriesChainID(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "n1", "https://p", "t")
	id, err := CreateForward(d, &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 20000, TargetIP: "5.6.7.8", TargetPort: 20001, Mode: "userspace"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := GetForward(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if f.ChainID.Valid {
		t.Fatalf("standalone forward should have NULL chain_id, got %+v", f.ChainID)
	}
	if f.Mode != "userspace" {
		t.Fatalf("mode = %q, want userspace", f.Mode)
	}

	// A forward tagged with a chain must round-trip that chain_id. Seed a chains
	// row directly so the chain-tagged forward's chain_id FK resolves.
	res, err := d.Exec(`INSERT INTO chains(name,proto,exit_host,exit_port,created_at) VALUES ('c','tcp','9.9.9.9',8443,0)`)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	cfID, err := CreateForward(d, &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 20001, TargetIP: "5.6.7.8", TargetPort: 20002, ChainID: sql.NullInt64{Int64: cid, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	cf, err := GetForward(d, cfID)
	if err != nil {
		t.Fatal(err)
	}
	if !cf.ChainID.Valid || cf.ChainID.Int64 != cid {
		t.Fatalf("chain forward chain_id = %+v, want valid %d", cf.ChainID, cid)
	}
}

func TestChainCRUD(t *testing.T) {
	d := openMemDB(t)
	id, err := CreateChain(d, &Chain{Name: "vless", Proto: "tcp", ExitHost: "seednet", ExitPort: 8443})
	if err != nil {
		t.Fatal(err)
	}
	c, err := GetChain(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "vless" || c.Proto != "tcp" || c.ExitHost != "seednet" || c.ExitPort != 8443 {
		t.Fatalf("round-trip mismatch: %+v", c)
	}
	if c.TenantID.Valid || c.EntryListenPort != 0 {
		t.Fatalf("fresh admin chain should have NULL tenant + entry 0: %+v", c)
	}
	admin, _ := ListAdminChains(d)
	if len(admin) != 1 {
		t.Fatalf("ListAdminChains = %d, want 1", len(admin))
	}
	nodes, err := DeleteChain(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 0 {
		t.Fatalf("no forwards yet, affected nodes should be empty: %v", nodes)
	}
	if _, err := GetChain(d, id); err == nil {
		t.Fatalf("chain should be gone after delete")
	}
}
