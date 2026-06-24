package landing

import (
	"io"
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

// NewFetcher returns a Fetcher with a sane HTTP timeout.
func NewFetcher() *Fetcher {
	return &Fetcher{
		client: &http.Client{Timeout: 10 * time.Second},
		cache:  map[string]cacheEntry{},
	}
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
