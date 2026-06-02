package db

import (
	"database/sql"
	"testing"
)

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

	// A forward tagged with a chain must round-trip that chain_id. CreateChain
	// does not exist yet, so seed the chains row directly.
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
