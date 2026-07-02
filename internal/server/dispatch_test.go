package server

import (
	"testing"

	"nft-forward/internal/db"
	"nft-forward/internal/nft"
)

func TestDispatchToNode_StoresWarningNotError(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	self, err := db.UpsertSelfNode(d)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: d, Dispatcher: &Dispatcher{
		DB:        d,
		SendLocal: func(rules []nft.Rule) (string, error) { return "1 条规则的目标无法解析：端口 8080 → bad.invalid", nil },
	}}
	if err := s.dispatchToNode(self.ID); err != nil {
		t.Fatalf("dispatch should succeed with warning: %v", err)
	}
	got, _ := db.GetNode(d, self.ID)
	if got.LastWarning == "" {
		t.Fatal("expected last_warning to be stored")
	}
	if got.LastError.Valid {
		t.Fatalf("last_error should stay clear, got %q", got.LastError.String)
	}
}
