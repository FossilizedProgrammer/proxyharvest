package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// geoClient resolves proxy IPs to ISO country codes with ip-api.com's free
// batch endpoint (100 addresses per request, 45 requests per minute).
type geoClient struct {
	client *http.Client
	mu     sync.Mutex
	cache  map[string]string
	last   time.Time
	logf   func(string, ...any)
	prog   *status
}

// newGeoClient returns a resolver that reaches ip-api.com through upstream when
// one is configured (country lookups are data fetches, not proxy tests).
func newGeoClient(upstream *url.URL, timeout time.Duration, logf func(string, ...any), prog *status) *geoClient {
	client := &http.Client{Timeout: timeout}
	if tr, err := newFetchTransport(upstream, timeout); err == nil {
		client.Transport = tr
	}
	return &geoClient{
		client: client,
		cache:  map[string]string{},
		logf:   logf,
		prog:   prog,
	}
}

type geoAnswer struct {
	Status      string `json:"status"`
	CountryCode string `json:"countryCode"`
	Query       string `json:"query"`
}

func (g *geoClient) resolve(ctx context.Context, ips []string) map[string]string {
	unique := make([]string, 0, len(ips))
	seen := map[string]bool{}
	for _, ip := range ips {
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		unique = append(unique, ip)
	}

	for start := 0; start < len(unique); start += 100 {
		end := start + 100
		if end > len(unique) {
			end = len(unique)
		}
		if g.prog != nil {
			g.prog.set("resolving proxy countries", fmt.Sprintf("%d/%d addresses", end, len(unique)))
		}
		g.lookupBatch(ctx, unique[start:end])
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]string, len(ips))
	for _, ip := range ips {
		if cc, ok := g.cache[ip]; ok {
			out[ip] = cc
		}
	}
	return out
}

func (g *geoClient) lookupBatch(ctx context.Context, ips []string) {
	g.mu.Lock()
	if wait := 4200*time.Millisecond - time.Since(g.last); wait > 0 {
		g.mu.Unlock()
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	} else {
		g.mu.Unlock()
	}

	payload := make([]map[string]string, len(ips))
	for i, ip := range ips {
		payload[i] = map[string]string{"query": ip}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}

	var answers []geoAnswer
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"http://ip-api.com/batch?fields=status,countryCode,query", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := g.client.Do(req)
		if err != nil {
			if attempt == 0 {
				continue
			}
			g.logf("country lookup failed: %v", err)
			return
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		g.mu.Lock()
		g.last = time.Now()
		g.mu.Unlock()
		if readErr != nil || resp.StatusCode != http.StatusOK {
			if attempt == 0 {
				time.Sleep(10 * time.Second)
				continue
			}
			g.logf("country lookup failed: HTTP %d", resp.StatusCode)
			return
		}
		if err := json.Unmarshal(data, &answers); err != nil {
			g.logf("country lookup returned malformed JSON: %v", err)
			return
		}
		break
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	for _, a := range answers {
		if a.Status == "success" && a.CountryCode != "" {
			g.cache[a.Query] = a.CountryCode
		}
	}
}

func (g *geoClient) resolved(ip string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cc, ok := g.cache[ip]
	return cc, ok
}
