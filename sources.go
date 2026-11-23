package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// TypeSet holds the proxy protocols the user wants to collect.
type TypeSet map[string]bool

func parseTypes(raw string) (TypeSet, error) {
	set := TypeSet{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		s := normalizeScheme(part)
		if s == "" {
			return nil, fmt.Errorf("unknown proxy type %q (use http, https, socks4 or socks5)", part)
		}
		set[s] = true
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("no proxy types selected")
	}
	return set, nil
}

// Request is one URL to download plus the parser for its payload.
type Request struct {
	URL   string
	Parse func(body []byte) []Candidate
}

// Source is a proxy-list website and the requests needed to harvest it.
type Source struct {
	Key      string
	Name     string
	Requests func(country string, types TypeSet) []Request
	Generic  bool
}

var reSchemed = regexp.MustCompile(`(?i)\b(https?|socks4a?|socks5)://(\d{1,3}(?:\.\d{1,3}){3}):(\d{1,5})\b`)
var reBare = regexp.MustCompile(`\b(\d{1,3}(?:\.\d{1,3}){3}):(\d{1,5})\b`)
var reSpys = regexp.MustCompile(`(?m)^(\d{1,3}(?:\.\d{1,3}){3}):(\d{1,5})\s+([A-Za-z]{2})-`)
var reFPL = regexp.MustCompile(`(?is)<td class='hd-ip'>\s*(\d{1,3}(?:\.\d{1,3}){3}):(\d{1,5})\s*</td>\s*<td>\s*([A-Za-z]{2})\s*</td>`)
var reFW = regexp.MustCompile(`(?s)<td[^>]*>\s*(\d{1,3}(?:\.\d{1,3}){3})\s*</td>\s*<td><a href="/\?port=(\d{1,5})">[^<]*</a></td>.*?class="flag flag-([a-z]{2})"`)
var rePL = regexp.MustCompile(`(?s)<td class="mono"[^>]*>\s*(\d{1,3}(?:\.\d{1,3}){3}):(\d{1,5})\s*</td>\s*<td><span class="badge[^"]*">\s*(HTTP|HTTPS|SOCKS4|SOCKS5)\s*</span>`)
var rePN = regexp.MustCompile(`(?s)<tr data-addr="(\d{1,3}(?:\.\d{1,3}){3}):(\d{1,5})".*?<span class="proto-pill">\s*([A-Za-z0-9]+)\s*</span>.*?<span class="mono"[^>]*>\s*([A-Z]{2})\s*</span>`)

func parseIPPortText(body []byte, scheme, source, country string) []Candidate {
	var out []Candidate
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		ip, port, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if c, ok := makeCandidate(ip, port, scheme, country, source); ok {
			out = append(out, c)
		}
	}
	return out
}

func parseGeneric(body []byte, source string) []Candidate {
	var out []Candidate
	seen := map[string]bool{}
	text := string(body)

	for _, m := range reSchemed.FindAllStringSubmatch(text, -1) {
		if c, ok := makeCandidate(m[2], m[3], m[1], "", source); ok && !seen[c.Key()] {
			seen[c.Key()] = true
			out = append(out, c)
		}
	}
	// Strip schemed endpoints so the bare pattern does not duplicate them as HTTP.
	stripped := reSchemed.ReplaceAllString(text, " ")
	for _, m := range reBare.FindAllStringSubmatch(stripped, -1) {
		if c, ok := makeCandidate(m[1], m[2], "http", "", source); ok && !seen[c.Key()] {
			seen[c.Key()] = true
			out = append(out, c)
		}
	}
	return out
}

func parseFreeProxyList(body []byte, source string) []Candidate {
	var out []Candidate
	for _, m := range reFPL.FindAllStringSubmatch(string(body), -1) {
		if c, ok := makeCandidate(m[1], m[2], "http", m[3], source); ok {
			out = append(out, c)
		}
	}
	return out
}

func parseSpys(body []byte, scheme, source string) []Candidate {
	var out []Candidate
	for _, m := range reSpys.FindAllStringSubmatch(string(body), -1) {
		if c, ok := makeCandidate(m[1], m[2], scheme, m[3], source); ok {
			out = append(out, c)
		}
	}
	return out
}

func parseFreeproxyWorld(body []byte, source string) []Candidate {
	var out []Candidate
	for _, m := range reFW.FindAllStringSubmatch(string(body), -1) {
		if c, ok := makeCandidate(m[1], m[2], "http", m[3], source); ok {
			out = append(out, c)
		}
	}
	return out
}

func parseProxylister(body []byte, source string) []Candidate {
	var out []Candidate
	for _, m := range rePL.FindAllStringSubmatch(string(body), -1) {
		if c, ok := makeCandidate(m[1], m[2], m[3], "", source); ok {
			out = append(out, c)
		}
	}
	return out
}

func parseProxynest(body []byte, source string) []Candidate {
	var out []Candidate
	for _, m := range rePN.FindAllStringSubmatch(string(body), -1) {
		if c, ok := makeCandidate(m[1], m[2], m[3], m[4], source); ok {
			out = append(out, c)
		}
	}
	return out
}

func parseGeonode(body []byte, source string) []Candidate {
	var payload struct {
		Data []struct {
			IP        string   `json:"ip"`
			Port      string   `json:"port"`
			Country   string   `json:"country"`
			Protocols []string `json:"protocols"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	var out []Candidate
	for _, row := range payload.Data {
		for _, proto := range row.Protocols {
			if c, ok := makeCandidate(row.IP, row.Port, proto, row.Country, source); ok {
				out = append(out, c)
			}
		}
	}
	return out
}

func parseFreeproxyhub(body []byte, source string) []Candidate {
	var payload struct {
		Proxies []struct {
			IP       string `json:"ip"`
			Port     int    `json:"port"`
			Country  string `json:"country"`
			Protocol string `json:"protocol"`
		} `json:"proxies"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	var out []Candidate
	for _, row := range payload.Proxies {
		if c, ok := makeCandidate(row.IP, strconv.Itoa(row.Port), row.Protocol, row.Country, source); ok {
			out = append(out, c)
		}
	}
	return out
}

func proxyscrapeRequests(country string, types TypeSet) []Request {
	var reqs []Request
	for _, proto := range []string{"http", "socks4", "socks5"} {
		if !types[proto] {
			continue
		}
		u := "https://api.proxyscrape.com/v4/free-proxy-list/get?request=display_proxies&proxy_format=ipport&format=text&protocol=" + proto
		if country != "" {
			u += "&country=" + country
		}
		scheme := proto
		reqs = append(reqs, Request{
			URL: u,
			Parse: func(body []byte) []Candidate {
				return parseIPPortText(body, scheme, "proxyscrape.com", country)
			},
		})
	}
	return reqs
}

func spysRequests(_ string, types TypeSet) []Request {
	var reqs []Request
	if types["http"] {
		reqs = append(reqs, Request{
			URL:   "https://spys.me/proxy.txt",
			Parse: func(body []byte) []Candidate { return parseSpys(body, "http", "spys.me") },
		})
	}
	if types["socks5"] {
		reqs = append(reqs, Request{
			URL:   "https://spys.me/socks.txt",
			Parse: func(body []byte) []Candidate { return parseSpys(body, "socks5", "spys.me") },
		})
	}
	return reqs
}

func geonodeRequests(country string, _ TypeSet) []Request {
	u := "https://proxylist.geonode.com/api/proxy-list?limit=500&page=1&sort_by=lastChecked&sort_type=desc"
	if country != "" {
		u += "&country=" + country
	}
	return []Request{{
		URL:   u,
		Parse: func(body []byte) []Candidate { return parseGeonode(body, "geonode.com") },
	}}
}

func freeproxyhubRequests(_ string, _ TypeSet) []Request {
	return []Request{{
		URL:   "https://freeproxyhub.net/api/v1/proxies?limit=500",
		Parse: func(body []byte) []Candidate { return parseFreeproxyhub(body, "freeproxyhub.net") },
	}}
}

func freeproxyWorldRequests(country string, _ TypeSet) []Request {
	u := "https://www.freeproxy.world/"
	if country != "" {
		u += "?country=" + country
	}
	return []Request{{
		URL:   u,
		Parse: func(body []byte) []Candidate { return parseFreeproxyWorld(body, "freeproxy.world") },
	}}
}

// Registry lists every built-in provider; no external sites file is needed.
var registry = []Source{
	{
		Key:  "free-proxy-list.net",
		Name: "free-proxy-list.net",
		Requests: func(string, TypeSet) []Request {
			return []Request{{
				URL:   "https://free-proxy-list.net/",
				Parse: func(body []byte) []Candidate { return parseFreeProxyList(body, "free-proxy-list.net") },
			}}
		},
	},
	{Key: "proxyscrape.com", Name: "proxyscrape.com", Requests: proxyscrapeRequests},
	{Key: "spys.me", Name: "spys.me / spys.one", Requests: spysRequests},
	{Key: "freeproxy.world", Name: "freeproxy.world", Requests: freeproxyWorldRequests},
	{
		Key:  "proxylister.com",
		Name: "proxylister.com",
		Requests: func(string, TypeSet) []Request {
			return []Request{{
				URL:   "https://proxylister.com/",
				Parse: func(body []byte) []Candidate { return parseProxylister(body, "proxylister.com") },
			}}
		},
	},
	{Key: "freeproxyhub.net", Name: "freeproxyhub.net", Requests: freeproxyhubRequests},
	{Key: "geonode.com", Name: "geonode.com", Requests: geonodeRequests},
	{
		Key:  "fineproxy.org",
		Name: "fineproxy.org",
		Requests: func(string, TypeSet) []Request {
			return []Request{{
				URL:   "https://fineproxy.org/free-proxy/",
				Parse: func(body []byte) []Candidate { return parseGeneric(body, "fineproxy.org") },
			}}
		},
	},
	{
		Key:  "databay.com",
		Name: "databay.com",
		Requests: func(string, TypeSet) []Request {
			return []Request{{
				URL:   "https://databay.com/free-proxy-list",
				Parse: func(body []byte) []Candidate { return parseGeneric(body, "databay.com") },
			}}
		},
	},
	{
		Key:  "proxynest.live",
		Name: "proxynest.live",
		Requests: func(string, TypeSet) []Request {
			return []Request{{
				URL:   "https://proxynest.live/proxy-list",
				Parse: func(body []byte) []Candidate { return parseProxynest(body, "proxynest.live") },
			}}
		},
	},
	{
		Key:  "premproxy.com",
		Name: "premproxy.com",
		Requests: func(string, TypeSet) []Request {
			return []Request{{
				URL:   "https://premproxy.com/proxy-by-country",
				Parse: func(body []byte) []Candidate { return parseGeneric(body, "premproxy.com") },
			}}
		},
	},
}

var sourceAliases = map[string]string{
	"spys.one":        "spys.me",
	"www.spys.me":     "spys.me",
	"www.spys.one":    "spys.me",
	"proxyscrape.com": "proxyscrape.com",
}

// printSources lists every built-in site together with the URLs it fetches,
// so users know what -sources accepts and where the proxies come from.
func printSources(types TypeSet) {
	fmt.Printf("# %d built-in proxy-list sites (pass names to -sources)\n", len(registry))
	width := 0
	for _, s := range registry {
		if len(s.Key) > width {
			width = len(s.Key)
		}
	}
	for _, s := range registry {
		urls := make([]string, 0, 4)
		for _, req := range s.Requests("", types) {
			urls = append(urls, req.URL)
		}
		label := fmt.Sprintf("%-*s", width, s.Key)
		if len(urls) == 0 {
			fmt.Printf("%s  (no request for the selected -types)\n", label)
			continue
		}
		fmt.Printf("%s  %s\n", label, urls[0])
		for _, u := range urls[1:] {
			fmt.Printf("%s  %s\n", strings.Repeat(" ", width), u)
		}
	}
}

// sourceKeys joins site keys for log lines.
func sourceKeys(sources []Source) string {
	keys := make([]string, 0, len(sources))
	for _, s := range sources {
		keys = append(keys, s.Key)
	}
	return strings.Join(keys, ", ")
}

// selectSources maps a comma-separated -sources value (source keys, aliases or
// display names) to registry entries. Empty or "all" means every source; a
// "!" or "-" prefix excludes a source instead of adding it.
func selectSources(raw string) ([]Source, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "all") {
		return append([]Source(nil), registry...), nil
	}

	pick := map[string]bool{}
	selected := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		exclude := false
		if strings.HasPrefix(part, "!") || strings.HasPrefix(part, "-") {
			exclude = true
			part = strings.TrimSpace(part[1:])
		}
		key, ok := resolveSourceKey(part)
		if !ok {
			return nil, fmt.Errorf("unknown source %q (see -list-sources for the valid names)", part)
		}
		if exclude {
			pick[key] = false
		} else if _, set := pick[key]; !set {
			pick[key] = true
		}
	}
	if len(pick) == 0 {
		return nil, fmt.Errorf("-sources is empty")
	}

	for _, s := range registry {
		include, set := pick[s.Key]
		if !set {
			include = !hasPositivePick(pick)
		}
		if include && !selected[s.Key] {
			selected[s.Key] = true
		}
	}

	var out []Source
	for _, s := range registry {
		if selected[s.Key] {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no sources left after applying -sources")
	}
	return out, nil
}

func hasPositivePick(pick map[string]bool) bool {
	for _, v := range pick {
		if v {
			return true
		}
	}
	return false
}

// resolveSourceKey normalizes a user-supplied source name to a registry key.
func resolveSourceKey(name string) (string, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimSuffix(n, "/")
	n = strings.TrimPrefix(n, "www.")
	if alias, ok := sourceAliases[n]; ok {
		n = alias
	}
	for _, s := range registry {
		if strings.ToLower(s.Key) == n || strings.ToLower(s.Name) == n {
			return s.Key, true
		}
	}
	return "", false
}

func sourceForHost(host string) Source {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if alias, ok := sourceAliases[host]; ok {
		host = alias
	}
	for _, s := range registry {
		if s.Key == host {
			return s
		}
	}
	// Unknown site: still try its home page with the generic extractor.
	generic := Source{
		Key:     host,
		Name:    host,
		Generic: true,
		Requests: func(string, TypeSet) []Request {
			return []Request{{
				URL:   "https://" + host + "/",
				Parse: func(body []byte) []Candidate { return parseGeneric(body, host) },
			}}
		},
	}
	return generic
}
