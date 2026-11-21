package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type options struct {
	sites        string
	country      string
	types        string
	concurrency  int
	timeout      time.Duration
	fetchTimeout time.Duration
	testURL      string
	out          string
	top          int
	maxPerSource int
	upstream     string
	noRetry      bool
	require      string
	excludeCC    string
	verbose      bool
	listSources  bool
}

type Result struct {
	Candidate
	Latency time.Duration
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("proxyharvest: ")
	if err := run(); err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}

func run() error {
	var opts options
	flag.StringVar(&opts.sites, "sites", "sites", "file containing proxy-list site addresses")
	flag.StringVar(&opts.country, "country", "", "only collect/test proxies from this ISO country code (e.g. US, DE, IR)")
	flag.StringVar(&opts.types, "types", "http,socks4,socks5", "comma-separated proxy types: http, https, socks4, socks5")
	flag.IntVar(&opts.concurrency, "c", 100, "parallel proxy tests")
	flag.DurationVar(&opts.timeout, "timeout", 8*time.Second, "timeout for each proxy test")
	flag.DurationVar(&opts.fetchTimeout, "fetch-timeout", 25*time.Second, "timeout for downloading a source list")
	flag.StringVar(&opts.testURL, "test-url", "https://www.gstatic.com/generate_204", "URL used to measure proxy speed (HTTP fallback is tried automatically)")
	flag.StringVar(&opts.out, "out", "proxies.txt", "output file, fastest first")
	flag.IntVar(&opts.top, "top", 0, "keep only the N fastest working proxies (0 = all)")
	flag.StringVar(&opts.upstream, "upstream", "", "upstream proxy for downloading source lists, never used for testing (http[s]://[user:pass@]host:port or socks5://[user:pass@]host:port)")
	flag.IntVar(&opts.maxPerSource, "max-per-source", 500, "maximum candidates taken from each site")
	flag.StringVar(&opts.require, "require", "www.facebook.com,www.youtube.com", "comma-separated sites every proxy must open over HTTPS; use \"none\" to disable")
	flag.StringVar(&opts.excludeCC, "exclude-countries", "IR,RU,CN,KP", "comma-separated ISO country codes to drop (use \"none\" to keep every country)")
	flag.BoolVar(&opts.noRetry, "no-retry", false, "skip unreachable sites instead of retrying them through harvested proxies")
	flag.BoolVar(&opts.verbose, "v", false, "verbose logging")
	flag.BoolVar(&opts.listSources, "list-sources", false, "print known proxy-list sites and exit")
	flag.Parse()

	if opts.listSources {
		for _, s := range registry {
			fmt.Printf("%-22s %s\n", s.Key, s.Name)
		}
		return nil
	}

	types, err := parseTypes(opts.types)
	if err != nil {
		return err
	}
	country := strings.ToUpper(strings.TrimSpace(opts.country))
	if country != "" && (len(country) != 2 || !isLetters(country)) {
		return fmt.Errorf("-country expects a two-letter ISO code such as US, DE or IR")
	}
	if opts.concurrency < 1 {
		return fmt.Errorf("-c must be at least 1")
	}
	if opts.timeout <= 0 || opts.fetchTimeout <= 0 {
		return fmt.Errorf("timeouts must be positive")
	}
	requiredSites, err := parseRequireSites(opts.require)
	if err != nil {
		return err
	}
	excludedCountries, err := parseCountryBlocklist(opts.excludeCC)
	if err != nil {
		return err
	}

	testURLs := splitList(opts.testURL)
	if len(testURLs) == 0 {
		return fmt.Errorf("-test-url is empty")
	}
	probes := probeURLs(testURLs)

	upstream, err := parseUpstream(opts.upstream)
	if err != nil {
		return err
	}

	logf := func(format string, args ...any) {
		if opts.verbose {
			log.Printf(format, args...)
		}
	}

	prog := newStatus(os.Stdout)
	prog.start()
	defer prog.stop()
	log.SetOutput(prog.LogWriter())

	hosts, err := loadSiteHosts(opts.sites)
	if err != nil {
		return fmt.Errorf("reading sites file: %w", err)
	}
	sources := make([]Source, 0, len(hosts))
	seenSources := map[string]bool{}
	for _, host := range hosts {
		src := sourceForHost(host)
		if seenSources[src.Key] {
			continue
		}
		seenSources[src.Key] = true
		sources = append(sources, src)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var jobs []fetchJob
	for _, src := range sources {
		for _, req := range src.Requests(country, types) {
			jobs = append(jobs, fetchJob{source: src, request: req})
		}
	}
	if len(jobs) == 0 {
		return fmt.Errorf("no fetchable sources for the selected proxy types")
	}

	log.Printf("collecting proxies from %d sites (country=%s, types=%s)",
		len(sources), orAll(country), strings.Join(sortedTypes(types), ","))
	if upstream != nil {
		log.Printf("downloading site lists through upstream proxy %s", upstream.Redacted())
	}

	outcomes := fetchJobs(ctx, jobs, upstream, minInt(8, len(jobs)), opts.fetchTimeout, prog)

	var (
		candidates []Candidate
		failed     []jobOutcome
		stats      = map[string][]string{}
	)
	for _, outcome := range outcomes {
		if outcome.err != nil {
			failed = append(failed, outcome)
			stats[outcome.job.source.Name] = append(stats[outcome.job.source.Name], "unreachable: "+outcome.err.Error())
			logf("source %s failed: %v", outcome.job.source.Name, outcome.err)
			continue
		}
		candidates = append(candidates, outcome.candidates...)
		logf("source %s: %d candidates", outcome.job.source.Name, len(outcome.candidates))
	}

	// Any source that could not be reached directly is retried through proxies
	// harvested from the sources that did work, unless -no-retry was given.
	if len(failed) > 0 && opts.noRetry {
		log.Printf("skipping %d unreachable source request(s) (-no-retry)", len(failed))
	} else if len(failed) > 0 && len(candidates) > 0 {
		log.Printf("%d source request(s) unreachable directly; retrying through harvested proxies", len(failed))
		pool := dedupeCandidates(append([]Candidate(nil), candidates...))
		bootstrapTimeout := opts.timeout
		if bootstrapTimeout > 6*time.Second {
			bootstrapTimeout = 6 * time.Second
		}
		proxies := workingProxies(ctx, pool, 8, 100, 20, bootstrapTimeout, probes, logf, prog)
		if len(proxies) == 0 {
			log.Printf("no working proxy available for the retry")
		} else {
			log.Printf("retrying through %d working proxies", len(proxies))
			extra, recovered := retryViaProxies(ctx, failed, proxies, opts.fetchTimeout, logf, prog)
			candidates = append(candidates, extra...)
			if recovered > 0 {
				log.Printf("recovered %d unreachable source request(s) via proxy", recovered)
			}
		}
	}

	candidates = dedupeCandidates(candidates)
	candidates = applyFilters(candidates, types, country)
	candidates = excludeByCountry(ctx, candidates, upstream, excludedCountries, opts.verbose, prog)
	candidates = capPerSource(candidates, opts.maxPerSource)

	if country != "" {
		before := len(candidates)
		candidates = filterByCountry(ctx, candidates, upstream, country, opts.verbose, prog)
		log.Printf("country filter %s: %d of %d candidates kept", country, len(candidates), before)
	}
	if len(candidates) == 0 {
		return fmt.Errorf("no candidate proxies found (see -v for source errors)")
	}

	log.Printf("testing %d proxies from direct egress (no outer proxy)", len(candidates))
	results := testCandidates(ctx, candidates, probes, opts.timeout, opts.concurrency, prog)

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("interrupted")
	}
	if len(results) == 0 {
		return fmt.Errorf("all %d tested proxies failed; nothing to write", len(candidates))
	}

	if len(requiredSites) > 0 {
		labels := strings.Join(siteLabels(requiredSites), ", ")
		before := len(results)
		log.Printf("checking %d working proxies can open %s", before, labels)
		results = verifySites(ctx, results, requiredSites, opts.timeout, opts.concurrency, logf, prog)
		log.Printf("%d of %d working proxies can open %s", len(results), before, labels)
		if len(results) == 0 {
			return fmt.Errorf("no proxy could open %s; nothing to write (use -require none to skip this check)", labels)
		}
	}

	// Fill in country codes for output where the source did not provide one.
	fillCountries(ctx, results, upstream, opts.verbose, prog)

	if len(excludedCountries) > 0 {
		kept := results[:0]
		for _, r := range results {
			if excludedCountries[r.Country] {
				continue
			}
			kept = append(kept, r)
		}
		if len(kept) != len(results) {
			log.Printf("country exclusion: dropped %d tested proxies (%s)", len(results)-len(kept), formatCountryList(excludedCountries))
		}
		results = kept
		if len(results) == 0 {
			return fmt.Errorf("all working proxies are from excluded countries (%s)", formatCountryList(excludedCountries))
		}
	}

	sort.SliceStable(results, func(i, j int) bool { return results[i].Latency < results[j].Latency })
	workingTotal := len(results)
	if opts.top > 0 && len(results) > opts.top {
		results = results[:opts.top]
	}

	prog.stop()
	if err := writeResults(opts.out, results, country); err != nil {
		return err
	}

	printResults(results)
	log.Printf("%d/%d proxies working; fastest %s; written to %s",
		workingTotal, len(candidates), results[0].URL(), opts.out)
	return nil
}

func testCandidates(ctx context.Context, candidates []Candidate, probes []string, timeout time.Duration, concurrency int, prog *status) []Result {
	var (
		results = make(chan Result, len(candidates))
		sem     = make(chan struct{}, concurrency)
		wg      sync.WaitGroup
		tested  atomic.Int64
		alive   atomic.Int64
	)
	for _, cand := range candidates {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(cand Candidate) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			latency, err := checkCandidate(ctx, cand, probes, timeout)
			n := tested.Add(1)
			if err == nil {
				alive.Add(1)
				results <- Result{Candidate: cand, Latency: latency}
			}
			prog.set("testing proxies", fmt.Sprintf("%d/%d tested, %d alive", n, len(candidates), alive.Load()))
			if n%250 == 0 {
				log.Printf("tested %d/%d (alive %d)", n, len(candidates), alive.Load())
			}
		}(cand)
	}
	wg.Wait()
	close(results)

	out := make([]Result, 0, alive.Load())
	for r := range results {
		out = append(out, r)
	}
	return out
}

func parseCountryBlocklist(raw string) (map[string]bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "none") {
		return nil, nil
	}
	set := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		code := strings.ToUpper(strings.TrimSpace(part))
		if code == "" {
			continue
		}
		if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
			return nil, fmt.Errorf("invalid country code %q (use two-letter ISO codes like IR, RU, CN)", part)
		}
		set[code] = true
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("no country left in -exclude-countries")
	}
	return set, nil
}

func formatCountryList(set map[string]bool) string {
	codes := make([]string, 0, len(set))
	for code := range set {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return strings.Join(codes, ",")
}

// excludeByCountry drops candidates from blocked countries. Countries missing
// from the source data are looked up, so e.g. an Iranian proxy cannot slip
// through just because its list did not label it.
func excludeByCountry(ctx context.Context, candidates []Candidate, upstream *url.URL, excluded map[string]bool, verbose bool, prog *status) []Candidate {
	if len(excluded) == 0 || len(candidates) == 0 {
		return candidates
	}

	var unknown []string
	for _, c := range candidates {
		if c.Country == "" {
			unknown = append(unknown, c.IP)
		}
	}
	resolved := map[string]string{}
	if len(unknown) > 0 {
		logf := func(format string, args ...any) {
			if verbose {
				log.Printf(format, args...)
			}
		}
		log.Printf("looking up country for %d proxies to apply -exclude-countries", len(uniqueStrings(unknown)))
		geo := newGeoClient(upstream, 15*time.Second, logf, prog)
		resolved = geo.resolve(ctx, unknown)
	}

	out := make([]Candidate, 0, len(candidates))
	dropped := map[string]int{}
	for _, c := range candidates {
		cc := c.Country
		if cc == "" {
			cc = resolved[c.IP]
		}
		if cc != "" && excluded[cc] {
			dropped[cc]++
			continue
		}
		c.Country = cc
		out = append(out, c)
	}
	if len(dropped) > 0 {
		parts := make([]string, 0, len(dropped))
		for code, n := range dropped {
			parts = append(parts, fmt.Sprintf("%s=%d", code, n))
		}
		sort.Strings(parts)
		log.Printf("country exclusion: dropped %d candidates (%s)", len(candidates)-len(out), strings.Join(parts, ", "))
	}
	return out
}

func filterByCountry(ctx context.Context, candidates []Candidate, upstream *url.URL, country string, verbose bool, prog *status) []Candidate {
	var unknownIPs []string
	for _, c := range candidates {
		if c.Country == "" {
			unknownIPs = append(unknownIPs, c.IP)
		}
	}

	logf := func(format string, args ...any) {
		if verbose {
			log.Printf(format, args...)
		}
	}
	resolved := map[string]string{}
	if len(unknownIPs) > 0 {
		log.Printf("looking up country for %d proxies without source country data", len(uniqueStrings(unknownIPs)))
		geo := newGeoClient(upstream, 15*time.Second, logf, prog)
		resolved = geo.resolve(ctx, unknownIPs)
	}

	out := make([]Candidate, 0, len(candidates))
	unresolved := 0
	for _, c := range candidates {
		cc := c.Country
		if cc == "" {
			cc = resolved[c.IP]
		}
		if cc == "" {
			unresolved++
			continue
		}
		if cc == country {
			c.Country = cc
			out = append(out, c)
		}
	}
	if unresolved > 0 {
		log.Printf("warning: dropped %d candidates whose country could not be verified", unresolved)
	}
	return out
}

func fillCountries(ctx context.Context, results []Result, upstream *url.URL, verbose bool, prog *status) {
	var missing []string
	for _, r := range results {
		if r.Country == "" {
			missing = append(missing, r.IP)
		}
	}
	if len(missing) == 0 {
		return
	}
	logf := func(format string, args ...any) {
		if verbose {
			log.Printf(format, args...)
		}
	}
	geo := newGeoClient(upstream, 15*time.Second, logf, prog)
	resolved := geo.resolve(ctx, missing)
	for i := range results {
		if results[i].Country == "" {
			results[i].Country = resolved[results[i].IP]
		}
	}
}

func capPerSource(in []Candidate, max int) []Candidate {
	if max <= 0 {
		return in
	}
	counts := map[string]int{}
	out := make([]Candidate, 0, len(in))
	for _, c := range in {
		if counts[c.Source] >= max {
			continue
		}
		counts[c.Source]++
		out = append(out, c)
	}
	return out
}

func writeResults(path string, results []Result, country string) error {
	var b strings.Builder
	b.WriteString("# proxyharvest: working proxies, fastest first\n")
	fmt.Fprintf(&b, "# generated: %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(&b, "# country: %s\n", orAll(country))
	b.WriteString("# columns: proxy_url\tcountry\tlatency_ms\tsource\n")
	for _, r := range results {
		cc := r.Country
		if cc == "" {
			cc = "-"
		}
		fmt.Fprintf(&b, "%s\t%s\t%d\t%s\n", r.URL(), cc, r.Latency.Milliseconds(), r.Source)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func printResults(results []Result) {
	limit := len(results)
	if limit > 20 {
		limit = 20
	}
	fmt.Printf("\n%-4s %-32s %-3s %-9s %s\n", "#", "proxy", "cc", "latency", "source")
	for i := 0; i < limit; i++ {
		r := results[i]
		cc := r.Country
		if cc == "" {
			cc = "-"
		}
		fmt.Printf("%-4d %-32s %-3s %-9s %s\n", i+1, r.URL(), cc, r.Latency.Round(time.Millisecond), r.Source)
	}
	if len(results) > limit {
		fmt.Printf("... %d more in the output file\n", len(results)-limit)
	}
	fmt.Println()
}

// parseUpstream validates the -upstream value and accepts a bare host:port
// (treated as an HTTP proxy). Credentials may be embedded in the URL.
func parseUpstream(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid -upstream value: %w", err)
	}
	switch u.Scheme {
	case "http", "https", "socks5":
	default:
		return nil, fmt.Errorf("-upstream supports http, https and socks5 proxies, not %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("-upstream needs a host, e.g. http://127.0.0.1:8080")
	}
	if _, port, err := net.SplitHostPort(u.Host); err != nil || port == "" {
		return nil, fmt.Errorf("-upstream needs an explicit port, e.g. http://127.0.0.1:8080")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("-upstream must not contain a path")
	}
	return u, nil
}

func splitList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func sortedTypes(types TypeSet) []string {
	out := make([]string, 0, len(types))
	for t := range types {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func isLetters(s string) bool {
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func orAll(country string) string {
	if country == "" {
		return "all"
	}
	return country
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
