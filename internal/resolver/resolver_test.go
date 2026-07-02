package resolver

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestIsHostname(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"1.2.3.4", false},
		{"home.example.ddns.net", true},
		{"::1", false},
		{"localhost", true},
		{"bad host", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsHostname(c.in); got != c.want {
			t.Errorf("IsHostname(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestLookupUsesInjectedFunc(t *testing.T) {
	called := 0
	r := &Resolver{
		Lookup: func(ctx context.Context, host string) ([]string, error) {
			called++
			return []string{"10.0.0.1"}, nil
		},
		Timeout: time.Second,
	}
	ip, err := r.LookupIPv4(context.Background(), "x.example")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.0.0.1" {
		t.Fatalf("got %q", ip)
	}
	if called != 1 {
		t.Fatalf("called=%d", called)
	}
}

func TestLookupSkipsIPv6(t *testing.T) {
	r := &Resolver{
		Lookup: func(ctx context.Context, host string) ([]string, error) {
			return []string{"::1", "2001:db8::1", "192.0.2.5"}, nil
		},
	}
	ip, err := r.LookupIPv4(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.0.2.5" {
		t.Fatalf("got %q", ip)
	}
}

func TestLookupNoIPv4(t *testing.T) {
	r := &Resolver{
		Lookup: func(ctx context.Context, host string) ([]string, error) {
			return []string{"::1"}, nil
		},
	}
	_, err := r.LookupIPv4(context.Background(), "x")
	if !errors.Is(err, ErrNoIPv4) {
		t.Fatalf("err=%v", err)
	}
}

func TestPlausibleHostname(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"example.com", true},
		{"sub.example.com.", true}, // 尾点 FQDN
		{"x-1.example.org", true},
		{"localhost", true},
		{"", false},
		{"4212", false},      // 纯数字端口误填为 host
		{"1.2.3.999", false}, // 末标签全数字（打错的 IP）
		{"1.2.3.4", false},   // 合法 IP 不是 hostname
		{"bad_host!", false}, // 非法字符
	}
	for _, c := range cases {
		if got := PlausibleHostname(c.in); got != c.want {
			t.Errorf("PlausibleHostname(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
