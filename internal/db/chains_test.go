package db

import (
	"database/sql"
	"testing"
)

func TestOccupiedPortsReturnsForwards(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "n1", "https://p", "t")
	if _, err := CreateForward(d, &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 20000, TargetIP: "1.1.1.1", TargetPort: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateForward(d, &Forward{NodeID: n.ID, Proto: "udp", ListenPort: 53, TargetIP: "8.8.8.8", TargetPort: 53}); err != nil {
		t.Fatal(err)
	}
	occ, err := OccupiedPortsOnNode(d, n.ID, "tcp", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !occ[20000] {
		t.Fatalf("tcp forward on 20000 should be occupied: %v", occ)
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

func TestCountForwardsByTunnel(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "n1", "https://p", "t")
	tunID, err := CreateTunnel(d, &Tunnel{Name: "a", NodeID: n.ID, ProtoMask: "tcp+udp", PortStart: 30000, PortEnd: 30100, TargetCIDRAllow: "0.0.0.0/0"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := CountForwardsByTunnel(d, tunID); err != nil || got != 0 {
		t.Fatalf("fresh tunnel should back 0 forwards, got %d (err %v)", got, err)
	}
	if _, err := CreateForward(d, &Forward{
		NodeID:     n.ID,
		TunnelID:   sql.NullInt64{Int64: tunID, Valid: true},
		Proto:      "tcp",
		ListenPort: 30000,
		TargetIP:   "10.0.0.1",
		TargetPort: 443,
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := CountForwardsByTunnel(d, tunID); err != nil || got != 1 {
		t.Fatalf("tunnel should back 1 forward after create, got %d (err %v)", got, err)
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

func TestChainsReferencingNode(t *testing.T) {
	d := openMemDB(t)
	a := chainTestNode(t, d, "node-a", "1.1.1.1")
	b := chainTestNode(t, d, "node-b", "2.2.2.2")
	other := chainTestNode(t, d, "node-c", "3.3.3.3") // not in the chain
	cid, _ := CreateChain(d, &Chain{Name: "vless", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 8443})
	c, _ := GetChain(d, cid)
	regen(t, d, c, []HopInput{{NodeID: a.ID}, {NodeID: b.ID}})

	for _, n := range []*Node{a, b} {
		refs, err := ChainsReferencingNode(d, n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) != 1 || refs[0] != cid {
			t.Fatalf("ChainsReferencingNode(%d) = %v, want [%d]", n.ID, refs, cid)
		}
	}

	refs, err := ChainsReferencingNode(d, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("ChainsReferencingNode(unrelated) = %v, want empty", refs)
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

// seedTwoHopChain creates an admin chain with hops on two fresh nodes and
// returns the chain plus both node IDs. Hop 0 targets hop 1; hop 1 targets
// the chain exit.
func seedTwoHopChain(t *testing.T, d *sql.DB) (*Chain, int64, int64) {
	t.Helper()
	n0, _ := CreateNode(d, "edge-0", "https://p0", "tok0")
	n1, _ := CreateNode(d, "edge-1", "https://p1", "tok1")
	// relay_host must be set or RegenerateChain rejects the hop.
	if _, err := d.Exec(`UPDATE nodes SET relay_host=? WHERE id=?`, "10.0.0.10", n0.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE nodes SET relay_host=? WHERE id=?`, "10.0.0.11", n1.ID); err != nil {
		t.Fatal(err)
	}
	c := &Chain{Name: "wire", Proto: "tcp", ExitHost: "9.9.9.9", ExitPort: 443}
	id, err := CreateChain(d, c)
	if err != nil {
		t.Fatal(err)
	}
	c.ID = id
	tx, _ := d.Begin()
	if _, _, err := RegenerateChain(tx, c, []HopInput{{NodeID: n0.ID}, {NodeID: n1.ID}}, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()
	return c, n0.ID, n1.ID
}

func TestRegenerateChainHonorsDesiredPortAndSyncsUpstream(t *testing.T) {
	d := openMemDB(t)
	c, n0, n1 := seedTwoHopChain(t, d)

	hops, _ := ListChainHops(d, c.ID)
	inputs := make([]HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = HopInput{NodeID: h.NodeID, TunnelID: h.TunnelID, Mode: h.Mode}
		if h.NodeID == n1 {
			inputs[i].DesiredPort = 21111
		}
	}
	tx, _ := d.Begin()
	if _, _, err := RegenerateChain(tx, c, inputs, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()

	fwds, _ := ListForwardsByChain(d, c.ID)
	byNode := map[int64]*Forward{}
	for _, f := range fwds {
		byNode[f.NodeID] = f
	}
	if byNode[n1].ListenPort != 21111 {
		t.Fatalf("hop 1 listen_port = %d, want 21111", byNode[n1].ListenPort)
	}
	if byNode[n0].TargetPort != 21111 {
		t.Fatalf("upstream hop 0 target_port = %d, want 21111 (must follow downstream)", byNode[n0].TargetPort)
	}
}

func TestRegenerateChainRejectsOutOfRangeDesiredPort(t *testing.T) {
	d := openMemDB(t)
	c, _, n1 := seedTwoHopChain(t, d)
	hops, _ := ListChainHops(d, c.ID)
	inputs := make([]HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = HopInput{NodeID: h.NodeID, TunnelID: h.TunnelID, Mode: h.Mode}
		if h.NodeID == n1 {
			inputs[i].DesiredPort = 80 // below ChainPortMin
		}
	}
	tx, _ := d.Begin()
	_, _, err := RegenerateChain(tx, c, inputs, nil)
	tx.Rollback()
	if err == nil {
		t.Fatal("expected out-of-range desired port to be rejected")
	}
}

func TestRegenerateChainRejectsOccupiedDesiredPort(t *testing.T) {
	d := openMemDB(t)
	c, _, n1 := seedTwoHopChain(t, d)
	// A non-chain forward already holds an in-range port on n1; pinning the hop
	// to it must surface a conflict rather than silently reallocate.
	const occupied = 21500
	if _, err := CreateForward(d, &Forward{NodeID: n1, Proto: c.Proto, ListenPort: occupied, TargetIP: "1.2.3.4", TargetPort: 9}); err != nil {
		t.Fatal(err)
	}
	hops, _ := ListChainHops(d, c.ID)
	inputs := make([]HopInput, len(hops))
	for i, h := range hops {
		inputs[i] = HopInput{NodeID: h.NodeID, TunnelID: h.TunnelID, Mode: h.Mode}
		if h.NodeID == n1 {
			inputs[i].DesiredPort = occupied
		}
	}
	tx, _ := d.Begin()
	_, _, err := RegenerateChain(tx, c, inputs, nil)
	tx.Rollback()
	if err == nil {
		t.Fatal("expected occupied desired port to be rejected")
	}
}

func TestRegenerateChainKeepsCustomCommentAcrossRegen(t *testing.T) {
	d := openMemDB(t)
	c, n0, n1 := seedTwoHopChain(t, d)

	hops, _ := ListChainHops(d, c.ID)
	mk := func(custom map[int64]string) []HopInput {
		in := make([]HopInput, len(hops))
		for i, h := range hops {
			in[i] = HopInput{NodeID: h.NodeID, TunnelID: h.TunnelID, Mode: h.Mode}
			if cm, ok := custom[h.NodeID]; ok {
				in[i].Comment = cm
			}
		}
		return in
	}
	tx, _ := d.Begin()
	if _, _, err := RegenerateChain(tx, c, mk(map[int64]string{n1: "my custom"}), nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()

	tx, _ = d.Begin()
	if _, _, err := RegenerateChain(tx, c, mk(nil), nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Commit()

	hops, _ = ListChainHops(d, c.ID)
	byNode := map[int64]*ChainHop{}
	for _, h := range hops {
		byNode[h.NodeID] = h
	}
	if byNode[n1].Comment != "my custom" {
		t.Fatalf("custom comment not preserved: %q", byNode[n1].Comment)
	}
	if byNode[n0].Comment != "" {
		t.Fatalf("non-custom hop should have empty chain_hops.comment, got %q", byNode[n0].Comment)
	}
	fwds, _ := ListForwardsByChain(d, c.ID)
	fwdByNode := map[int64]*Forward{}
	for _, f := range fwds {
		fwdByNode[f.NodeID] = f
	}
	// The preserved custom value must propagate to forwards.comment, not just
	// linger on chain_hops.
	if fwdByNode[n1].Comment != "my custom" {
		t.Fatalf("custom comment did not reach forwards.comment: %q", fwdByNode[n1].Comment)
	}
	if fwdByNode[n0].Comment == "" {
		t.Fatalf("forwards.comment for default hop should be generated, got empty")
	}
}
