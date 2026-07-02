package server

import "testing"

func TestParseExitBareIPv6Hint(t *testing.T) {
	_, _, err := parseExit("2001:db8::1:1080")
	if err == nil {
		t.Fatal("expected error for bare IPv6 without brackets")
	}
	want := "IPv6 地址需要用方括号包裹，例如 [::1]:1080"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
}

func TestParseExitBracketedIPv6Succeeds(t *testing.T) {
	host, port, err := parseExit("[2001:db8::1]:1080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "2001:db8::1" || port != 1080 {
		t.Fatalf("got host=%q port=%d, want host=2001:db8::1 port=1080", host, port)
	}
}

func TestParseExitGenericFormatError(t *testing.T) {
	_, _, err := parseExit("not-an-address")
	if err == nil {
		t.Fatal("expected error for malformed input")
	}
	want := "出口需为 host:port 形式"
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err.Error(), want)
	}
}

func TestParseExitValidIPv4(t *testing.T) {
	host, port, err := parseExit("10.0.0.1:80")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "10.0.0.1" || port != 80 {
		t.Fatalf("got host=%q port=%d, want host=10.0.0.1 port=80", host, port)
	}
}

func TestParseExit(t *testing.T) {
	cases := []struct {
		raw     string
		wantErr bool
	}{
		{"1.2.3.4:80", false},
		{"example.com:443", false},
		{"[2001:db8::1]:80", false},
		{"4212:80", true},  // 纯数字 host —— 被误填的端口
		{"host:0", true},   // 端口非法
		{":80", true},      // host 空
		{"nohostport", true},
	}
	for _, c := range cases {
		_, _, err := parseExit(c.raw)
		if (err != nil) != c.wantErr {
			t.Errorf("parseExit(%q) err = %v, wantErr = %v", c.raw, err, c.wantErr)
		}
	}
}
