package portutil

import "testing"

func TestPickFreePortInRange(t *testing.T) {
	p := PickFreePort(100, 110, map[int]bool{})
	if p < 100 || p > 110 {
		t.Fatalf("port %d out of [100,110]", p)
	}
}

func TestPickFreePortAvoidsUsed(t *testing.T) {
	used := map[int]bool{}
	for i := 100; i <= 109; i++ {
		used[i] = true
	}
	if p := PickFreePort(100, 110, used); p != 110 {
		t.Fatalf("only 110 free, got %d", p)
	}
}

func TestPickFreePortExhausted(t *testing.T) {
	if p := PickFreePort(100, 101, map[int]bool{100: true, 101: true}); p != 0 {
		t.Fatalf("want 0 when exhausted, got %d", p)
	}
}

func TestPickFreePortInvertedRange(t *testing.T) {
	if p := PickFreePort(110, 100, map[int]bool{}); p != 0 {
		t.Fatalf("want 0 for inverted range, got %d", p)
	}
}
