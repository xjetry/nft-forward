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
