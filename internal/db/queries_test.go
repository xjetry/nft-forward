package db

import "testing"

func TestNodeLastWarningRoundTrip(t *testing.T) {
	d := openTestDB(t)

	n, err := UpsertSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}

	if err := MarkNodeApplied(d, n.ID, "2 条规则的目标无法解析：端口 8080 → 4212"); err != nil {
		t.Fatal(err)
	}
	got, _ := GetNode(d, n.ID)
	if got.LastWarning == "" {
		t.Fatal("last_warning should be set after MarkNodeApplied with warning")
	}
	if got.LastError.Valid {
		t.Fatal("last_error should be cleared on apply")
	}

	// 干净成功清 warning
	if err := MarkNodeApplied(d, n.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = GetNode(d, n.ID)
	if got.LastWarning != "" {
		t.Fatalf("last_warning should be cleared, got %q", got.LastWarning)
	}

	// 下发硬失败：置 error、清 warning
	_ = MarkNodeApplied(d, n.ID, "some warning")
	if err := MarkNodeDispatchError(d, n.ID, "boom"); err != nil {
		t.Fatal(err)
	}
	got, _ = GetNode(d, n.ID)
	if got.LastWarning != "" {
		t.Fatalf("dispatch error should clear warning, got %q", got.LastWarning)
	}
	if !got.LastError.Valid || got.LastError.String != "boom" {
		t.Fatalf("last_error = %+v, want boom", got.LastError)
	}
}

func TestNodeRelayHostDeclaredRoundTrip(t *testing.T) {
	d := openTestDB(t)
	n, err := CreateNode(d, "n1", "https://p", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if n.RelayHostDeclared || n.RelayHostV6Declared {
		t.Fatalf("new node should start undeclared, got %+v", n)
	}

	if err := UpdateNodeRelayHost(d, n.ID, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	if err := SetNodeRelayHostDeclared(d, n.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := SetNodeRelayHostV6Declared(d, n.ID, true); err != nil {
		t.Fatal(err)
	}

	got, err := GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.RelayHostDeclared {
		t.Error("RelayHostDeclared should be true after SetNodeRelayHostDeclared(true)")
	}
	if !got.RelayHostV6Declared {
		t.Error("RelayHostV6Declared should be true after SetNodeRelayHostV6Declared(true)")
	}

	if err := SetNodeRelayHostDeclared(d, n.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err = GetNode(d, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RelayHostDeclared {
		t.Error("RelayHostDeclared should be false after SetNodeRelayHostDeclared(false)")
	}
	if !got.RelayHostV6Declared {
		t.Error("RelayHostV6Declared should remain true (only the v4 flag was cleared)")
	}
}
