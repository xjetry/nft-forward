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
