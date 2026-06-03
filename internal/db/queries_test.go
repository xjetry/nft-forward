package db

import (
	"database/sql"
	"testing"
)

func openMemDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestUpsertSelfNodeIsIdempotent(t *testing.T) {
	d := openMemDB(t)
	n1, err := UpsertSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}
	n2, err := UpsertSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}
	if n1.ID != n2.ID || n1.NodeKind != "self" || n2.Address != "unix:///var/run/nft-forward.sock" {
		t.Fatalf("self-node not idempotent: %+v vs %+v", n1, n2)
	}
	var cnt int
	if err := d.QueryRow(`SELECT COUNT(*) FROM nodes WHERE node_kind='self'`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Fatalf("expected exactly 1 self node row, got %d", cnt)
	}
}

func TestMarkNodeOnlineUpdatesFields(t *testing.T) {
	d := openMemDB(t)
	n, err := CreateNode(d, "edge-1", "https://panel.example.com", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkNodeOnline(d, n.ID, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	got, err := GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Online != 1 || got.AgentVersion != "v1.2.3" || got.LastSeen == nil {
		t.Fatalf("MarkNodeOnline did not update fields: %+v", got)
	}
}

func TestMarkLocalMigratedSetsTimestamp(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "e1", "https://p", "t")
	if got, _ := GetNode(d, n.ID); got.LocalMigratedAt != nil {
		t.Fatalf("expected nil LocalMigratedAt initially")
	}
	stamped, err := MarkLocalMigrated(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stamped {
		t.Fatal("expected first call to stamp")
	}
	got, _ := GetNode(d, n.ID)
	if got.LocalMigratedAt == nil {
		t.Fatalf("expected LocalMigratedAt to be set")
	}
	// Second call must report no-op.
	stamped, err = MarkLocalMigrated(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stamped {
		t.Fatal("expected second call to be no-op (already migrated)")
	}
}

func TestDispatchOutcomeRecording(t *testing.T) {
	d := openMemDB(t)
	n, err := CreateNode(d, "edge-1", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	// Failure first.
	if err := MarkNodeDispatchError(d, n.ID, "node not connected"); err != nil {
		t.Fatal(err)
	}
	got, _ := GetNode(d, n.ID)
	if !got.LastError.Valid || got.LastError.String != "node not connected" {
		t.Fatalf("expected last_error stored, got %+v", got.LastError)
	}
	if got.LastApplyAt.Valid {
		t.Fatalf("LastApplyAt should still be unset after failure")
	}
	// Now success: last_error clears, last_apply_at is stamped.
	if err := MarkNodeApplied(d, n.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = GetNode(d, n.ID)
	if got.LastError.Valid {
		t.Fatalf("last_error should be cleared on success, got %q", got.LastError.String)
	}
	if !got.LastApplyAt.Valid {
		t.Fatalf("LastApplyAt should be set on success")
	}
}

func TestForward_ModeRoundTrip(t *testing.T) {
	d := openMemDB(t)
	n, err := CreateNode(d, "edge-1", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}
	f := &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 8443, TargetIP: "10.0.0.1", TargetPort: 443, Mode: "userspace"}
	id, err := CreateForward(d, f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := GetForward(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "userspace" {
		t.Fatalf("mode lost: %q", got.Mode)
	}

	// An empty mode must normalize to the kernel default (the column is
	// NOT NULL with a kernel default).
	def := &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 8444, TargetIP: "10.0.0.1", TargetPort: 443}
	defID, err := CreateForward(d, def)
	if err != nil {
		t.Fatal(err)
	}
	gotDef, err := GetForward(d, defID)
	if err != nil {
		t.Fatal(err)
	}
	if gotDef.Mode != "kernel" {
		t.Fatalf("empty mode should default to kernel, got %q", gotDef.Mode)
	}
}

func TestTuiSnapshotRoundTrip(t *testing.T) {
	d := openMemDB(t)
	n, err := CreateNode(d, "edge-1", "https://p", "tok")
	if err != nil {
		t.Fatal(err)
	}

	// No snapshot yet: returns ("", nil, nil).
	got, ts, err := GetTuiSnapshot(d, n.ID)
	if err != nil {
		t.Fatalf("GetTuiSnapshot before any upsert: %v", err)
	}
	if got != "" || ts != nil {
		t.Fatalf("expected empty snapshot before upsert, got %q ts=%v", got, ts)
	}

	// First upsert: insert.
	payload1 := `[{"proto":"tcp","listen_port":80,"target_ip":"10.0.0.1","target_port":80}]`
	if err := UpsertTuiSnapshot(d, n.ID, payload1); err != nil {
		t.Fatal(err)
	}
	got, ts, err = GetTuiSnapshot(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != payload1 || ts == nil {
		t.Fatalf("first read: got=%q ts=%v", got, ts)
	}

	// Second upsert with different payload: overwrite, not duplicate.
	payload2 := `[{"proto":"udp","listen_port":53,"target_ip":"10.0.0.2","target_port":53}]`
	if err := UpsertTuiSnapshot(d, n.ID, payload2); err != nil {
		t.Fatal(err)
	}
	got, _, _ = GetTuiSnapshot(d, n.ID)
	if got != payload2 {
		t.Fatalf("second read should reflect overwrite, got=%q", got)
	}

	// Verify exactly one row exists (no duplicate path).
	var cnt int
	if err := d.QueryRow(`SELECT COUNT(*) FROM node_tui_snapshot WHERE node_id=?`, n.ID).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Fatalf("expected exactly 1 snapshot row after overwrite, got %d", cnt)
	}
}

func TestUpdateForward_NonChainRowUpdatesEditableFields(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "edge-1", "https://p", "tok")
	id, err := CreateForward(d, &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 30000, TargetIP: "10.0.0.1", TargetPort: 30000, Comment: "old", Mode: "kernel"})
	if err != nil {
		t.Fatal(err)
	}

	affected, err := UpdateForward(d, n.ID, "tcp", 30000, "10.9.9.9", 8443, "new", "userspace")
	if err != nil {
		t.Fatal(err)
	}
	if affected != 1 {
		t.Fatalf("expected 1 row affected, got %d", affected)
	}
	got, err := GetForward(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetIP != "10.9.9.9" || got.TargetPort != 8443 || got.Comment != "new" || got.Mode != "userspace" {
		t.Fatalf("editable fields not updated: %+v", got)
	}
}

func TestUpdateForward_ChainRowIsNotTouched(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "edge-1", "https://p", "tok")
	// Seed a chains row directly so the chain-tagged forward's chain_id FK resolves.
	res, err := d.Exec(`INSERT INTO chains(name,proto,exit_host,exit_port,created_at) VALUES ('c','tcp','9.9.9.9',8443,0)`)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := res.LastInsertId()
	id, err := CreateForward(d, &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 20001, TargetIP: "5.6.7.8", TargetPort: 20002, ChainID: sql.NullInt64{Int64: cid, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}

	affected, err := UpdateForward(d, n.ID, "tcp", 20001, "1.1.1.1", 1, "hijack", "userspace")
	if err != nil {
		t.Fatal(err)
	}
	if affected != 0 {
		t.Fatalf("chain row must not be updated, got %d rows affected", affected)
	}
	got, err := GetForward(d, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetIP != "5.6.7.8" || got.TargetPort != 20002 {
		t.Fatalf("chain row port/target must stay intact: %+v", got)
	}
}

func TestUpdateForward_EmptyModeNormalizesToKernel(t *testing.T) {
	d := openMemDB(t)
	n, _ := CreateNode(d, "edge-1", "https://p", "tok")
	id, _ := CreateForward(d, &Forward{NodeID: n.ID, Proto: "tcp", ListenPort: 30001, TargetIP: "10.0.0.1", TargetPort: 30001, Mode: "userspace"})

	if _, err := UpdateForward(d, n.ID, "tcp", 30001, "10.0.0.1", 30001, "", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := GetForward(d, id)
	if got.Mode != "kernel" {
		t.Fatalf("empty mode should normalize to kernel, got %q", got.Mode)
	}
}
