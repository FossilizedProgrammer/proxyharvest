package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Candidate is one proxy endpoint harvested from a source.
type Candidate struct {
	IP      string
	Port    string
	Scheme  string // http, socks4, socks5
	Country string // ISO 3166-1 alpha-2, empty when unknown
	Source  string
}

func (c Candidate) Addr() string {
	return net.JoinHostPort(c.IP, c.Port)
}

func (c Candidate) URL() string {
	return c.Scheme + "://" + c.Addr()
}

func (c Candidate) Key() string {
	return c.Scheme + "://" + c.IP + ":" + c.Port
}

// normalizeScheme collapses HTTPS (CONNECT-capable HTTP proxies) to http.
func normalizeScheme(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "http", "https":
		return "http"
	case "socks4", "socks4a":
		return "socks4"
	case "socks5", "socks":
		return "socks5"
	}
	return ""
}

func validOctets(ip string) bool {
	parsed := net.ParseIP(ip)
	return parsed != nil && parsed.To4() != nil
}

func isPublicIPv4(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return false
	}
	if parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsUnspecified() ||
		parsed.IsMulticast() || parsed.IsLinkLocalUnicast() {
		return false
	}
	// 0.0.0.0/8 and 100.64.0.0/10 (carrier-grade NAT) are not usable proxies.
	if parsed[12] == 0 {
		return false
	}
	if parsed[12] == 100 && parsed[13] >= 64 && parsed[13] <= 127 {
		return false
	}
	return true
}

func validPort(port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n <= 65535
}

func makeCandidate(ip, port, scheme, country, source string) (Candidate, bool) {
	scheme = normalizeScheme(scheme)
	if scheme == "" || !isPublicIPv4(ip) || !validPort(port) {
		return Candidate{}, false
	}
	if port[0] == '0' {
		return Candidate{}, false
	}
	return Candidate{
		IP:      ip,
		Port:    port,
		Scheme:  scheme,
		Country: strings.ToUpper(strings.TrimSpace(country)),
		Source:  source,
	}, true
}

func (c Candidate) String() string {
	return fmt.Sprintf("%s://%s (%s)", c.Scheme, c.Addr(), c.Source)
}
