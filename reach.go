package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// parseRequireSites turns -require into full HTTPS URLs. "none" (or an empty
// value) disables the check.
func parseRequireSites(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "none") {
		return nil, nil
	}

	var targets []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "://") {
			part = "https://" + part
		}
		u, err := url.Parse(part)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("invalid -require site %q", part)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("invalid -require site %q: use http or https", part)
		}
		u.Fragment = ""
		key := u.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		targets = append(targets, key)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no site left in -require")
	}
	return targets, nil
}

func siteLabel(target string) string {
	if u, err := url.Parse(target); err == nil && u.Host != "" {
		return u.Host
	}
	return target
}

func siteLabels(targets []string) []string {
	labels := make([]string, len(targets))
	for i, target := range targets {
		labels[i] = siteLabel(target)
	}
	return labels
}

// describeReachError turns TLS verification failures into a clear reason, so
// intercepted proxies are not mistaken for plain timeouts.
func describeReachError(err error) string {
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	if errors.As(err, &unknownAuthority) {
		return "TLS interception: untrusted certificate issuer (MITM proxy)"
	}
	if errors.As(err, &hostname) {
		return "TLS interception: certificate does not match the site name (MITM proxy)"
	}
	if errors.As(err, &invalidCert) {
		return "TLS interception: invalid certificate (MITM proxy)"
	}
	return err.Error()
}

// checkReachability opens every target through cand over HTTPS, like a browser
// would. A proxy that cannot tunnel TLS to these hosts is useless in practice.
func checkReachability(ctx context.Context, cand Candidate, targets []string, timeout time.Duration) error {
	tr, err := newTransport(&cand, timeout)
	if err != nil {
		return err
	}
	defer tr.CloseIdleConnections()

	client := &http.Client{Transport: tr}
	for _, target := range targets {
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
		if err != nil {
			cancel()
			return err
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			return fmt.Errorf("%s: %s", siteLabel(target), describeReachError(err))
		}
		_, _ = io.CopyN(io.Discard, resp.Body, 4096)
		resp.Body.Close()
		cancel()

		// Defensive: never accept a connection whose certificate chain was not
		// verified. A proxy that intercepts TLS shows up here (or as a handshake
		// error above) and would break the site in a browser with HSTS.
		if resp.TLS == nil || len(resp.TLS.VerifiedChains) == 0 {
			return fmt.Errorf("%s: TLS connection was not verified (possible interception)", siteLabel(target))
		}

		// 5xx and 407 mean the proxy itself failed; other 4xx pages are still
		// real pages served by the site (login walls, consent pages, ...).
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusProxyAuthRequired {
			return fmt.Errorf("%s: status %d", siteLabel(target), resp.StatusCode)
		}
	}
	return nil
}

// verifySites keeps only the results whose proxy can open all required sites.
func verifySites(ctx context.Context, results []Result, targets []string, timeout time.Duration, concurrency int, logf func(string, ...any), prog *status) []Result {
	if len(targets) == 0 || len(results) == 0 {
		return results
	}
	if concurrency < 1 {
		concurrency = 1
	}

	labels := strings.Join(siteLabels(targets), ", ")
	prog.setProgress("checking required sites", 0, int64(len(results)), "0 can open "+labels)

	var (
		mu        sync.Mutex
		passed    []Result
		intercept int
		tested    atomic.Int64
		sem       = make(chan struct{}, concurrency)
		wg        sync.WaitGroup
	)
	for _, result := range results {
		wg.Add(1)
		go func(result Result) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			n := tested.Add(1)
			if err := checkReachability(ctx, result.Candidate, targets, timeout); err != nil {
				if strings.Contains(err.Error(), "interception") {
					mu.Lock()
					intercept++
					mu.Unlock()
				}
				logf("%s cannot open %s: %v", result.Candidate.URL(), labels, err)
			} else {
				mu.Lock()
				passed = append(passed, result)
				mu.Unlock()
			}
			mu.Lock()
			ok := len(passed)
			mu.Unlock()
			prog.setProgress("checking required sites", n, int64(len(results)), fmt.Sprintf("%d can open %s", ok, labels))
		}(result)
	}
	wg.Wait()

	if intercept > 0 {
		logf("%d proxies intercept TLS with an untrusted certificate and were dropped", intercept)
	}

	sort.SliceStable(passed, func(i, j int) bool { return passed[i].Latency < passed[j].Latency })
	return passed
}
