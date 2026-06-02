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
	admin, err := ListAdminChains(d)
	if err != nil {
		t.Fatal(err)
	}
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

func TestDeleteChainReturnsAffectedNodes(t *testing.T) {
	d := openMemDB(t)
	n, err := CreateNode(d, "n1", "https://p", "t")
	if err != nil {
		t.Fatal(err)
	}
	cid, err := CreateChain(d, &Chain{Name: "c", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443})
	if err != nil {
		t.Fatal(err)
	}
	f := &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 20000, TargetIP: "1.1.1.1", TargetPort: 1}
	f.ChainID = sql.NullInt64{Int64: cid, Valid: true}
	if _, err := CreateForward(d, f); err != nil {
		t.Fatal(err)
	}
	nodes, err := DeleteChain(d, cid)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0] != n.ID {
		t.Fatalf("affected nodes = %v, want [%d]", nodes, n.ID)
	}
	// FK cascade on forwards.chain_id must clear the chain's forwards.
	rest, err := ListForwardsByChain(d, cid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 {
		t.Fatalf("chain forwards should be gone after delete: %v", rest)
	}
}

func TestListChainsByTenant(t *testing.T) {
	d := openMemDB(t)
	tid, err := CreateTenant(d, &Tenant{Name: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	owned, err := CreateChain(d, &Chain{TenantID: sql.NullInt64{Int64: tid, Valid: true}, Name: "owned", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateChain(d, &Chain{Name: "admin", Proto: "tcp", ExitHost: "8.8.8.8", ExitPort: 443}); err != nil {
		t.Fatal(err)
	}
	byTenant, err := ListChainsByTenant(d, tid)
	if err != nil {
		t.Fatal(err)
	}
	if len(byTenant) != 1 || byTenant[0].ID != owned {
		t.Fatalf("ListChainsByTenant = %+v, want only chain %d", byTenant, owned)
	}
	// ListAdminChains filters tenant_id IS NULL, so it must exclude the tenant chain.
	admin, err := ListAdminChains(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range admin {
		if c.ID == owned {
			t.Fatalf("ListAdminChains must not include tenant-owned chain %d", owned)
		}
	}
}

// chainTestNode creates a node with a relay_host set so it can join a chain.
func chainTestNode(t *testing.T, d *sql.DB, name, relay string) *Node {
	t.Helper()
	n, err := CreateNode(d, name, "https://p", name+"-tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := UpdateNodeRelayHost(d, n.ID, relay); err != nil {
		t.Fatal(err)
	}
	got, _ := GetNode(d, n.ID)
	return got
}

func regen(t *testing.T, d *sql.DB, c *Chain, hops []HopInput) (string, []int64) {
	t.Helper()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	entry, affected, err := RegenerateChain(tx, c, hops, nil)
	if err != nil {
		tx.Rollback()
		t.Fatalf("RegenerateChain: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return entry, affected
}

func TestRegenerateThreeHopWiring(t *testing.T) {
	d := openMemDB(t)
	g := chainTestNode(t, d, "gomami", "1.1.1.1")
	h := chainTestNode(t, d, "nnc-hk", "2.2.2.2")
	w := chainTestNode(t, d, "nnc-tw", "3.3.3.3")
	cid, _ := CreateChain(d, &Chain{Name: "vless", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443})
	c, _ := GetChain(d, cid)

	entry, affected := regen(t, d, c, []HopInput{
		{NodeID: g.ID, Mode: "userspace"},
		{NodeID: h.ID, Mode: "userspace"},
		{NodeID: w.ID, Mode: "kernel"},
	})

	fws, _ := ListForwardsByChain(d, cid)
	if len(fws) != 3 {
		t.Fatalf("want 3 hop forwards, got %d", len(fws))
	}
	byNode := map[int64]*Forward{}
	for _, f := range fws {
		byNode[f.NodeID] = f
	}
	// 末跳打到出口
	if byNode[w.ID].TargetIP != "9.9.9.9" || byNode[w.ID].TargetPort != 8443 {
		t.Fatalf("last hop must target exit, got %s:%d", byNode[w.ID].TargetIP, byNode[w.ID].TargetPort)
	}
	// 中间跳打到下一跳的 relay_host:下一跳监听端口
	if byNode[g.ID].TargetIP != "2.2.2.2" || byNode[g.ID].TargetPort != byNode[h.ID].ListenPort {
		t.Fatalf("hop1 must target hop2 relay:port, got %s:%d (hop2 listen %d)", byNode[g.ID].TargetIP, byNode[g.ID].TargetPort, byNode[h.ID].ListenPort)
	}
	if byNode[h.ID].TargetIP != "3.3.3.3" || byNode[h.ID].TargetPort != byNode[w.ID].ListenPort {
		t.Fatalf("hop2 must target hop3 relay:port")
	}
	// 入口 = 第一跳 relay_host:监听端口
	wantEntry := hostPort("1.1.1.1", byNode[g.ID].ListenPort)
	if entry != wantEntry {
		t.Fatalf("entry = %q, want %q", entry, wantEntry)
	}
	if len(affected) != 3 {
		t.Fatalf("affected nodes = %d, want 3", len(affected))
	}
	// 模式逐跳：g/h userspace、w kernel
	if byNode[g.ID].Mode != "userspace" || byNode[w.ID].Mode != "kernel" {
		t.Fatalf("per-hop mode not honored: g=%s w=%s", byNode[g.ID].Mode, byNode[w.ID].Mode)
	}
}

func TestRegenerateRejectsMissingRelayHost(t *testing.T) {
	d := openMemDB(t)
	g := chainTestNode(t, d, "gomami", "1.1.1.1")
	bare, _ := CreateNode(d, "bare", "https://p", "x") // 无 relay_host
	cid, _ := CreateChain(d, &Chain{Name: "c", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443})
	c, _ := GetChain(d, cid)
	tx, _ := d.Begin()
	_, _, err := RegenerateChain(tx, c, []HopInput{{NodeID: g.ID}, {NodeID: bare.ID}}, nil)
	tx.Rollback()
	if err == nil {
		t.Fatalf("expected error for node without relay_host")
	}
}

func TestRegenerateRejectsRepeatedNode(t *testing.T) {
	d := openMemDB(t)
	g := chainTestNode(t, d, "gomami", "1.1.1.1")
	cid, _ := CreateChain(d, &Chain{Name: "c", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443})
	c, _ := GetChain(d, cid)
	tx, _ := d.Begin()
	_, _, err := RegenerateChain(tx, c, []HopInput{{NodeID: g.ID}, {NodeID: g.ID}}, nil)
	tx.Rollback()
	if err == nil {
		t.Fatalf("expected error for repeated node")
	}
}

func TestRegenerateKeepsPortOnReorder(t *testing.T) {
	d := openMemDB(t)
	g := chainTestNode(t, d, "gomami", "1.1.1.1")
	h := chainTestNode(t, d, "nnc-hk", "2.2.2.2")
	cid, _ := CreateChain(d, &Chain{Name: "c", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443})
	c, _ := GetChain(d, cid)
	regen(t, d, c, []HopInput{{NodeID: g.ID}, {NodeID: h.ID}})
	before, _ := ListForwardsByChain(d, cid)
	portByNode := map[int64]int{}
	for _, f := range before {
		portByNode[f.NodeID] = f.ListenPort
	}
	// 交换顺序：节点未变，各自端口应保留
	c, _ = GetChain(d, cid)
	regen(t, d, c, []HopInput{{NodeID: h.ID}, {NodeID: g.ID}})
	after, _ := ListForwardsByChain(d, cid)
	for _, f := range after {
		if portByNode[f.NodeID] != f.ListenPort {
			t.Fatalf("node %d port changed on reorder: %d -> %d", f.NodeID, portByNode[f.NodeID], f.ListenPort)
		}
	}
}

func TestRegenerateUDPForcesKernel(t *testing.T) {
	d := openMemDB(t)
	g := chainTestNode(t, d, "gomami", "1.1.1.1")
	h := chainTestNode(t, d, "nnc-hk", "2.2.2.2")
	cid, _ := CreateChain(d, &Chain{Name: "c", Proto: "udp", ExitHost: "9.9.9.9", ExitPort: 53})
	c, _ := GetChain(d, cid)
	regen(t, d, c, []HopInput{{NodeID: g.ID, Mode: "userspace"}, {NodeID: h.ID, Mode: "userspace"}})
	fws, _ := ListForwardsByChain(d, cid)
	for _, f := range fws {
		if f.Mode != "kernel" {
			t.Fatalf("udp hop must be kernel, got %s", f.Mode)
		}
	}
}

func TestRegenerateSingleHop(t *testing.T) {
	d := openMemDB(t)
	g := chainTestNode(t, d, "gomami", "1.1.1.1")
	cid, _ := CreateChain(d, &Chain{Name: "solo", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443})
	c, _ := GetChain(d, cid)

	entry, affected := regen(t, d, c, []HopInput{{NodeID: g.ID}})
	if len(affected) != 1 || affected[0] != g.ID {
		t.Fatalf("affected nodes = %v, want [%d]", affected, g.ID)
	}

	fws, _ := ListForwardsByChain(d, cid)
	if len(fws) != 1 {
		t.Fatalf("want 1 hop forward, got %d", len(fws))
	}
	hop := fws[0]
	// The lone hop is also the last hop, so it must target the chain's exit.
	if hop.TargetIP != "9.9.9.9" || hop.TargetPort != 8443 {
		t.Fatalf("single hop must target exit, got %s:%d", hop.TargetIP, hop.TargetPort)
	}
	// Entry = the node's relay_host : its listen_port.
	wantEntry := hostPort("1.1.1.1", hop.ListenPort)
	if entry != wantEntry {
		t.Fatalf("entry = %q, want %q", entry, wantEntry)
	}
	// entry_* must be persisted to the chains row.
	got, _ := GetChain(d, cid)
	if !got.EntryNodeID.Valid || got.EntryNodeID.Int64 != g.ID || got.EntryListenPort != hop.ListenPort {
		t.Fatalf("persisted entry = (node %+v, port %d), want (node %d, port %d)", got.EntryNodeID, got.EntryListenPort, g.ID, hop.ListenPort)
	}
	// entry_* must also be set on the passed-in *Chain.
	if !c.EntryNodeID.Valid || c.EntryNodeID.Int64 != g.ID || c.EntryListenPort != hop.ListenPort {
		t.Fatalf("in-memory entry = (node %+v, port %d), want (node %d, port %d)", c.EntryNodeID, c.EntryListenPort, g.ID, hop.ListenPort)
	}
}

func TestRegenerateAvoidForcesRealloc(t *testing.T) {
	d := openMemDB(t)
	g := chainTestNode(t, d, "gomami", "1.1.1.1")
	h := chainTestNode(t, d, "nnc-hk", "2.2.2.2")
	cid, _ := CreateChain(d, &Chain{Name: "c", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443})
	c, _ := GetChain(d, cid)
	regen(t, d, c, []HopInput{{NodeID: g.ID}, {NodeID: h.ID}})
	before, _ := ListForwardsByChain(d, cid)
	portByNode := map[int64]int{}
	for _, f := range before {
		portByNode[f.NodeID] = f.ListenPort
	}

	// Forcing g off its current port must reallocate g while h keeps its port.
	c, _ = GetChain(d, cid)
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = RegenerateChain(tx, c, []HopInput{{NodeID: g.ID}, {NodeID: h.ID}}, map[int64]int{g.ID: portByNode[g.ID]})
	if err != nil {
		tx.Rollback()
		t.Fatalf("RegenerateChain: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	after, _ := ListForwardsByChain(d, cid)
	portAfter := map[int64]int{}
	for _, f := range after {
		portAfter[f.NodeID] = f.ListenPort
	}
	if portAfter[g.ID] == portByNode[g.ID] {
		t.Fatalf("avoid should have forced node %d off port %d, but it kept it", g.ID, portByNode[g.ID])
	}
	if portAfter[h.ID] != portByNode[h.ID] {
		t.Fatalf("node %d port should be stable, changed %d -> %d", h.ID, portByNode[h.ID], portAfter[h.ID])
	}
}
