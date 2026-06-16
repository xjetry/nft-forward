package db

import "testing"

func TestParsePortRange(t *testing.T) {
	tests := []struct {
		input    string
		wantSegs int
		wantErr  bool
	}{
		// single range
		{"10001-20000", 1, false},
		// multiple ranges
		{"10001-19999,23333,40000-42000", 3, false},
		// single port
		{"23333", 1, false},
		// whitespace tolerance
		{" 10001 - 19999 , 23333 , 40000 - 42000 ", 3, false},
		// empty string defaults to DefaultPortRange
		{"", 1, false},
		// edge: port 1
		{"1", 1, false},
		// edge: port 65535
		{"65535", 1, false},
		// edge: full range
		{"1-65535", 1, false},

		// errors
		{"0", 0, true},           // port < 1
		{"65536", 0, true},       // port > 65535
		{"abc", 0, true},         // non-numeric
		{"100-50", 0, true},      // start > end
		{"0-100", 0, true},       // start out of range
		{"100-65536", 0, true},   // end out of range
		{",,,", 0, true},         // all empty => empty port range
	}
	for _, tt := range tests {
		segs, err := ParsePortRange(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParsePortRange(%q): expected error, got %v", tt.input, segs)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePortRange(%q): unexpected error: %v", tt.input, err)
			continue
		}
		if len(segs) != tt.wantSegs {
			t.Errorf("ParsePortRange(%q): got %d segments, want %d", tt.input, len(segs), tt.wantSegs)
		}
	}
}

func TestParsePortRangeValues(t *testing.T) {
	segs, err := ParsePortRange("10001-19999,23333,40000-42000")
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]int{{10001, 19999}, {23333, 23333}, {40000, 42000}}
	if len(segs) != len(want) {
		t.Fatalf("got %d segments, want %d", len(segs), len(want))
	}
	for i, s := range segs {
		if s != want[i] {
			t.Errorf("seg[%d] = %v, want %v", i, s, want[i])
		}
	}
}

func TestValidatePortRange(t *testing.T) {
	if err := ValidatePortRange("10001-20000"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := ValidatePortRange("abc"); err == nil {
		t.Error("expected error for invalid input")
	}
	// empty is valid (defaults)
	if err := ValidatePortRange(""); err != nil {
		t.Errorf("unexpected error for empty: %v", err)
	}
}

func TestPortInRange(t *testing.T) {
	segs := [][2]int{{10001, 19999}, {23333, 23333}, {40000, 42000}}
	tests := []struct {
		port int
		want bool
	}{
		{10001, true},
		{15000, true},
		{19999, true},
		{20000, false},
		{23333, true},
		{23334, false},
		{40000, true},
		{41000, true},
		{42000, true},
		{42001, false},
		{9999, false},
		{1, false},
	}
	for _, tt := range tests {
		got := PortInRange(tt.port, segs)
		if got != tt.want {
			t.Errorf("PortInRange(%d) = %v, want %v", tt.port, got, tt.want)
		}
	}
}

func TestPickFreePortFromRange(t *testing.T) {
	// Single port, not occupied
	segs := [][2]int{{12345, 12345}}
	p := PickFreePortFromRange(segs, nil)
	if p != 12345 {
		t.Errorf("got %d, want 12345", p)
	}

	// Single port, occupied => 0
	p = PickFreePortFromRange(segs, map[int]bool{12345: true})
	if p != 0 {
		t.Errorf("got %d, want 0 (exhausted)", p)
	}

	// Small range: 100-102 with 100 and 102 occupied => must return 101
	segs = [][2]int{{100, 102}}
	used := map[int]bool{100: true, 102: true}
	p = PickFreePortFromRange(segs, used)
	if p != 101 {
		t.Errorf("got %d, want 101", p)
	}

	// Fully exhausted range
	used = map[int]bool{100: true, 101: true, 102: true}
	p = PickFreePortFromRange(segs, used)
	if p != 0 {
		t.Errorf("got %d, want 0 (exhausted)", p)
	}

	// Multi-segment: first segment full, second has room
	segs = [][2]int{{10, 10}, {20, 22}}
	used = map[int]bool{10: true, 20: true, 21: true}
	p = PickFreePortFromRange(segs, used)
	if p != 22 {
		t.Errorf("got %d, want 22", p)
	}

	// nil used map => picks from range
	segs = [][2]int{{5000, 5002}}
	p = PickFreePortFromRange(segs, nil)
	if p < 5000 || p > 5002 {
		t.Errorf("got %d, want in [5000,5002]", p)
	}
}

func TestPickFreePortFromRangeDistribution(t *testing.T) {
	// Verify that with a range of 3 ports, all ports can be picked (randomness)
	segs := [][2]int{{100, 102}}
	seen := map[int]bool{}
	for i := 0; i < 300; i++ {
		p := PickFreePortFromRange(segs, nil)
		if p < 100 || p > 102 {
			t.Fatalf("port %d out of range", p)
		}
		seen[p] = true
	}
	// With 300 iterations and 3 choices, statistically all should appear
	if len(seen) != 3 {
		t.Errorf("expected all 3 ports to appear, got %d distinct: %v", len(seen), seen)
	}
}
