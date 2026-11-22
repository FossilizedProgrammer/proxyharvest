package main

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

const (
	userAgent  = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"
	maxBodyLen = 8 << 20
)

// newFetchTransport builds the transport used to download source lists. It
// egresses directly unless an upstream proxy was supplied; that upstream is
// only ever used for fetching, never for testing harvested proxies.
func newFetchTransport(upstream *url.URL, timeout time.Duration) (*http.Transport, error) {
	tr, err := newTransport(nil, timeout)
	if err != nil {
		return nil, err
	}
	if upstream != nil {
		tr.Proxy = http.ProxyURL(upstream)
	}
	return tr, nil
}

// fetchBody downloads u. When proxy is nil the connection goes through the
// configured upstream (or directly); otherwise the request is routed through
// that harvested proxy (used for sources that block us).
func fetchBody(ctx context.Context, u string, upstream *url.URL, proxy *Candidate, timeout time.Duration) ([]byte, error) {
	var (
		tr  *http.Transport
		err error
	)
	if proxy != nil {
		tr, err = newTransport(proxy, timeout)
	} else {
		tr, err = newFetchTransport(upstream, timeout)
	}
	if err != nil {
		return nil, err
	}
	defer tr.CloseIdleConnections()

	client := &http.Client{Transport: tr}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/json,text/plain,*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyLen+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyLen {
		return nil, fmt.Errorf("response larger than %d bytes", maxBodyLen)
	}
	return body, nil
}

type fetchJob struct {
	source  Source
	request Request
}

type jobOutcome struct {
	job        fetchJob
	candidates []Candidate
	err        error
}

// fetchJobs runs all jobs concurrently through the upstream proxy (or directly
// when upstream is nil).
func fetchJobs(ctx context.Context, jobs []fetchJob, upstream *url.URL, concurrency int, timeout time.Duration, prog *status) []jobOutcome {
	results := make([]jobOutcome, len(jobs))
	sem := make(chan struct{}, concurrency)
	var (
		wg        sync.WaitGroup
		done      atomic.Int64
		harvested atomic.Int64
	)
	prog.setProgress("downloading source lists", 0, int64(len(jobs)), "0 candidates")
	for i, job := range jobs {
		wg.Add(1)
		go func(i int, job fetchJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			body, err := fetchBody(ctx, job.request.URL, upstream, nil, timeout)
			cands := 0
			if err != nil {
				results[i] = jobOutcome{job: job, err: err}
				prog.note(fmt.Sprintf("%s: failed", job.source.Name))
			} else {
				results[i] = jobOutcome{job: job, candidates: job.request.Parse(body)}
				cands = len(results[i].candidates)
				prog.note(fmt.Sprintf("%s: %d candidates", job.source.Name, cands))
			}
			n := done.Add(1)
			total := harvested.Add(int64(cands))
			prog.setProgress("downloading source lists", n, int64(len(jobs)), fmt.Sprintf("%d candidates", total))
		}(i, job)
	}
	wg.Wait()
	return results
}

// workingProxies tests up to maxTries harvested candidates and returns the
// first `want` that respond. The proxies themselves are checked directly.
func workingProxies(ctx context.Context, pool []Candidate, want, maxTries, concurrency int, timeout time.Duration, probe []string, logf func(string, ...any), prog *status) []Candidate {
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	if len(pool) > maxTries {
		pool = pool[:maxTries]
	}

	var (
		mu     sync.Mutex
		alive  []Candidate
		found  atomic.Int64
		tested atomic.Int64
		sem    = make(chan struct{}, concurrency)
		wg     sync.WaitGroup
	)
	prog.setProgress("checking harvested proxies", 0, int64(len(pool)), "0 working")
	for _, cand := range pool {
		if found.Load() >= int64(want) {
			break
		}
		wg.Add(1)
		go func(cand Candidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if found.Load() >= int64(want) {
				return
			}
			latency, err := checkCandidate(ctx, cand, probe, timeout)
			n := tested.Add(1)
			if err == nil {
				mu.Lock()
				if int(found.Load()) < want {
					alive = append(alive, cand)
					found.Add(1)
					logf("bootstrap proxy works: %s", cand.URL())
					prog.note(fmt.Sprintf("working %s (%s)", cand.URL(), latency.Round(time.Millisecond)))
				}
				mu.Unlock()
			}
			prog.setProgress("checking harvested proxies", n, int64(len(pool)), fmt.Sprintf("%d working", found.Load()))
		}(cand)
	}
	wg.Wait()
	return alive
}

// retryViaProxies re-fetches failed jobs through proxies harvested in the first
// pass. It returns candidates and the number of jobs recovered.
func retryViaProxies(ctx context.Context, failed []jobOutcome, proxies []Candidate, timeout time.Duration, logf func(string, ...any), prog *status) ([]Candidate, int) {
	var (
		mu        sync.Mutex
		harvested []Candidate
		recovered int
		done      atomic.Int64
		wg        sync.WaitGroup
		sem       = make(chan struct{}, 4)
	)
	prog.setProgress("recovering unreachable sources", 0, int64(len(failed)), "via proxy")
	for _, outcome := range failed {
		wg.Add(1)
		go func(outcome jobOutcome) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			defer func() {
				prog.setProgress("recovering unreachable sources", done.Add(1), int64(len(failed)), "via proxy")
			}()

			for _, proxy := range proxies {
				body, err := fetchBody(ctx, outcome.job.request.URL, nil, &proxy, timeout)
				if err != nil {
					continue
				}
				cands := outcome.job.request.Parse(body)
				mu.Lock()
				harvested = append(harvested, cands...)
				recovered++
				mu.Unlock()
				logf("recovered %s via %s (%d proxies)", outcome.job.source.Name, proxy.URL(), len(cands))
				prog.note(fmt.Sprintf("recovered %s via %s", outcome.job.source.Name, proxy.URL()))
				return
			}
			logf("could not reach %s even through proxy", outcome.job.source.Name)
		}(outcome)
	}
	wg.Wait()
	return harvested, recovered
}

// dedupeCandidates keeps the first occurrence of each scheme://ip:port.
func dedupeCandidates(in []Candidate) []Candidate {
	seen := make(map[string]bool, len(in))
	out := make([]Candidate, 0, len(in))
	for _, c := range in {
		key := c.Key()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

// applyFilters drops wrong protocols and, when a country is requested, entries
// whose source-declared country is known to be different.
func applyFilters(in []Candidate, types TypeSet, country string) []Candidate {
	out := make([]Candidate, 0, len(in))
	for _, c := range in {
		if !types[c.Scheme] {
			continue
		}
		if country != "" && c.Country != "" && c.Country != country {
			continue
		}
		out = append(out, c)
	}
	return out
}

func probeURLs(testURLs []string) []string {
	out := make([]string, 0, len(testURLs)*2)
	seen := map[string]bool{}
	for _, u := range testURLs {
		for _, candidate := range []string{u, flipScheme(u)} {
			if candidate == "" || seen[candidate] {
				continue
			}
			seen[candidate] = true
			out = append(out, candidate)
		}
	}
	return out
}

func flipScheme(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "http"
	case "http":
		u.Scheme = "https"
	default:
		return ""
	}
	return u.String()
}
