package resolver

import (
	"context"
	"errors"
	"net"
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
