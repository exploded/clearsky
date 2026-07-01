package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// imgProxy fetches whitelisted upstream images server-side so the browser only
// ever talks to our own HTTPS origin. This kills mixed-content warnings (some
// sources are HTTP-only), avoids third-party requests from the client, and lets
// us cache. Responses are held in-memory with a short TTL; on an upstream error
// we serve the last good copy rather than a broken image.
type imgProxy struct {
	client  *http.Client
	sources map[string]string // source key -> upstream URL (the whitelist)
	ttl     time.Duration

	mu    sync.Mutex
	cache map[string]cachedImg
}

type cachedImg struct {
	body        []byte
	contentType string
	fetchedAt   time.Time
}

const maxImgBytes = 8 << 20 // 8 MiB ceiling per upstream image

func newImgProxy(sources map[string]string) *imgProxy {
	return &imgProxy{
		client:  &http.Client{Timeout: 15 * time.Second},
		sources: sources,
		ttl:     5 * time.Minute,
		cache:   make(map[string]cachedImg),
	}
}

// handle serves GET /img/{source}. Unknown sources 404 (the whitelist is the
// only thing that gets proxied — no user-controlled URLs).
func (p *imgProxy) handle(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("source")
	upstream, ok := p.sources[key]
	if !ok {
		http.NotFound(w, r)
		return
	}
	img, err := p.get(r.Context(), key, upstream)
	if err != nil {
		slog.Error("img proxy fetch", "source", key, "err", err)
		http.Error(w, "upstream image unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", img.contentType)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(img.body)
}

// get returns a fresh cached image, fetching from upstream when the TTL has
// lapsed. On a fetch failure it falls back to a stale cached copy if one exists.
func (p *imgProxy) get(ctx context.Context, key, upstream string) (cachedImg, error) {
	p.mu.Lock()
	cached, hasCached := p.cache[key]
	p.mu.Unlock()
	if hasCached && time.Since(cached.fetchedAt) < p.ttl {
		return cached, nil
	}

	img, err := p.fetch(ctx, upstream)
	if err != nil {
		if hasCached {
			slog.Warn("img proxy serving stale after fetch error", "source", key, "err", err)
			return cached, nil
		}
		return cachedImg{}, err
	}

	p.mu.Lock()
	p.cache[key] = img
	p.mu.Unlock()
	return img, nil
}

func (p *imgProxy) fetch(ctx context.Context, upstream string) (cachedImg, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream, nil)
	if err != nil {
		return cachedImg{}, err
	}
	req.Header.Set("User-Agent", "clearsky-astro/1.0 (+https://clearsky.mchugh.au)")

	resp, err := p.client.Do(req)
	if err != nil {
		return cachedImg{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return cachedImg{}, fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImgBytes))
	if err != nil {
		return cachedImg{}, err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	return cachedImg{body: body, contentType: ct, fetchedAt: time.Now()}, nil
}
