package resolver

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"
)

var (
	ErrNoIPv4 = errors.New("no IPv4 address for host")
	ErrNoIPv6 = errors.New("no IPv6 address for host")
)

// IsHostname returns true when s looks like a DNS name rather than a literal IP.
// Empty or syntactically invalid strings return false so callers reject them early.
func IsHostname(s string) bool {
	if s == "" {
		return false
	}
	if net.ParseIP(s) != nil {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '.' || r == '_':
		default:
			return false
		}
	}
	return true
}

// PlausibleHostname reports whether s could ever resolve as a DNS name. It
// builds on IsHostname (which already rejects IPs and illegal characters) and
// additionally rejects a name whose rightmost label is all-numeric: a numeric
// TLD can never exist, so such a string is a user error (a bare port like
// "4212", or a mistyped address like "1.2.3.999") rather than a resolvable host.
func PlausibleHostname(s string) bool {
	if !IsHostname(s) {
		return false
	}
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return false
	}
	labels := strings.Split(s, ".")
	last := labels[len(labels)-1]
	if last == "" {
		return false
	}
	for _, r := range last {
		if r < '0' || r > '9' {
			return true
		}
	}
	return false
}

// Resolver wraps net.LookupHost so tests can stub the network call.
type Resolver struct {
	Lookup  func(ctx context.Context, host string) ([]string, error)
	Timeout time.Duration
}

func New() *Resolver {
	return &Resolver{
		Lookup: func(ctx context.Context, host string) ([]string, error) {
			return net.DefaultResolver.LookupHost(ctx, host)
		},
		Timeout: 3 * time.Second,
	}
}

func (r *Resolver) LookupIPv4(ctx context.Context, host string) (string, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	addrs, err := r.Lookup(ctx, host)
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip != nil && ip.To4() != nil {
			return ip.To4().String(), nil
		}
	}
	return "", ErrNoIPv4
}

func (r *Resolver) LookupIPv6(ctx context.Context, host string) (string, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	addrs, err := r.Lookup(ctx, host)
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip != nil && ip.To4() == nil {
			return ip.String(), nil
		}
	}
	return "", ErrNoIPv6
}
