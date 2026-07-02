package landing

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// cacheTTL bounds how long a fetched subscription is reused. Rendering rule
// lists/details resolves landing sets often; the cache keeps that from hitting
// the upstream panel on every request while staying fresh enough for edits.
const cacheTTL = 60 * time.Second

type cacheEntry struct {
	nodes   []Node
	fetched time.Time
	err     error
}

// Fetcher fetches and parses subscription URLs, caching results per URL.
type Fetcher struct {
	client *http.Client
	mu     sync.Mutex
	cache  map[string]cacheEntry
}

// NewFetcher returns a Fetcher with a sane HTTP timeout. Subscription URLs are
// user-supplied, so the fetch is an SSRF surface: the client resolves+validates
// the target at connect time (DNS-rebinding-safe) and bounds redirects so it
// can't be pointed at loopback, private, link-local (incl. 169.254.169.254
// cloud metadata) or ULA addresses.
func NewFetcher() *Fetcher {
	return &Fetcher{
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("重定向次数过多")
				}
				return nil
			},
			Transport: &http.Transport{DialContext: guardedDialContext},
		},
		cache: map[string]cacheEntry{},
	}
}

// guardedDialContext resolves the target and refuses to dial if any resolved IP
// is non-public, then dials the validated IP literal directly so a DNS rebind
// between check and connect can't slip a private address through.
func guardedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("无法解析主机 %q", host)
	}
	for _, ip := range ips {
		if !isPublicIP(ip.IP) {
			return nil, fmt.Errorf("拒绝访问非公网地址 %s", ip.IP)
		}
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	var lastErr error
	for _, ip := range ips {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// isPublicIP reports whether ip is a routable public address. It rejects
// loopback, private (RFC1918 / ULA fc00::/7), link-local (169.254/16 and
// fe80::/10, which covers cloud metadata), CGNAT (100.64/10), unspecified and
// multicast ranges.
func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return false // 100.64.0.0/10 CGNAT
	}
	return true
}

// Subscription returns the landing nodes for a subscription URL. Results are
// cached for cacheTTL; pass force=true to bypass the cache and refetch.
func (f *Fetcher) Subscription(url string, force bool) ([]Node, error) {
	f.mu.Lock()
	if !force {
		if e, ok := f.cache[url]; ok && time.Since(e.fetched) < cacheTTL {
			f.mu.Unlock()
			return e.nodes, e.err
		}
	}
	f.mu.Unlock()

	nodes, err := f.fetch(url)
	f.mu.Lock()
	f.cache[url] = cacheEntry{nodes: nodes, fetched: time.Now(), err: err}
	f.mu.Unlock()
	return nodes, err
}

func (f *Fetcher) fetch(url string) ([]Node, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// Some panels switch format on User-Agent; a generic client gets the default
	// base64 URI list, which DecodeSubscription handles.
	req.Header.Set("User-Agent", "nft-forward")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return ParseURIs(DecodeSubscription(body)), nil
}
