package daemon

import (
	"strings"
	"testing"

	"nft-forward/internal/nft"
)

func TestMergedRuleset_EmptyOwnersReturnsEmpty(t *testing.T) {
	got, err := MergedRuleset(OwnerRuleset{})
	if err != nil {
		t.Fatalf("merge empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %d", len(got))
	}
}

func TestMergedRuleset_SingleOwnerPassesThrough(t *testing.T) {
	in := OwnerRuleset{
		"tui": []nft.Rule{
			{ID: "a", Proto: "tcp", SrcPort: 80, DestIP: "1.2.3.4", DestPort: 80},
			{ID: "b", Proto: "udp", SrcPort: 53, DestIP: "8.8.8.8", DestPort: 53},
		},
	}
	got, err := MergedRuleset(in)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rules, got %d: %+v", len(got), got)
	}
}

func TestMergedRuleset_MultiOwnerDeterministicOrder(t *testing.T) {
	in := OwnerRuleset{
		"panel": []nft.Rule{{ID: "p1", Proto: "tcp", SrcPort: 90, DestIP: "1.0.0.0", DestPort: 90}},
		"tui":   []nft.Rule{{ID: "t1", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80}},
	}
	got, err := MergedRuleset(in)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(got))
	}
	if got[0].ID != "p1" || got[1].ID != "t1" {
		t.Fatalf("merge order not deterministic by owner name: %+v", got)
	}
}

func TestMergedRuleset_CrossOwnerSamePortConflicts(t *testing.T) {
	in := OwnerRuleset{
		"tui":   []nft.Rule{{ID: "a", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80}},
		"panel": []nft.Rule{{ID: "b", Proto: "tcp", SrcPort: 80, DestIP: "2.0.0.0", DestPort: 80}},
	}
	_, err := MergedRuleset(in)
	if err == nil {
		t.Fatal("expected conflict error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tcp/80") {
		t.Errorf("error should name the conflicting port; got: %s", msg)
	}
	if !strings.Contains(msg, "tui") || !strings.Contains(msg, "panel") {
		t.Errorf("error should name both owners; got: %s", msg)
	}
}

func TestMergedRuleset_SameOwnerSamePortConflicts(t *testing.T) {
	in := OwnerRuleset{
		"tui": []nft.Rule{
			{ID: "a", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80},
			{ID: "b", Proto: "tcp", SrcPort: 80, DestIP: "2.0.0.0", DestPort: 80},
		},
	}
	_, err := MergedRuleset(in)
	if err == nil {
		t.Fatal("expected intra-owner conflict error")
	}
	if !strings.Contains(err.Error(), "tcp/80") {
		t.Errorf("error should name the conflicting port; got: %s", err.Error())
	}
}

func TestMergedRuleset_DifferentProtoSamePortOK(t *testing.T) {
	in := OwnerRuleset{
		"tui": []nft.Rule{
			{ID: "a", Proto: "tcp", SrcPort: 80, DestIP: "1.0.0.0", DestPort: 80},
			{ID: "b", Proto: "udp", SrcPort: 80, DestIP: "2.0.0.0", DestPort: 80},
		},
	}
	got, err := MergedRuleset(in)
	if err != nil {
		t.Fatalf("tcp+udp on same port should not conflict: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both rules, got %d", len(got))
	}
}
