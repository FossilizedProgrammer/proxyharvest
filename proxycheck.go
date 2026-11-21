package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// newTransport builds an HTTP transport that egresses directly and, when cand
// is non-nil, routes traffic through that one proxy. Nothing else is chained.
func newTransport(cand *Candidate, timeout time.Duration) (*http.Transport, error) {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: -1}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: false},
	}
	if cand == nil {
		return tr, nil
	}

	switch cand.Scheme {
	case "http":
		tr.Proxy = http.ProxyURL(&url.URL{Scheme: "http", Host: cand.Addr()})
	case "socks4":
		tr.DialContext = (&socks4Dialer{proxyAddr: cand.Addr(), timeout: timeout}).DialContext
	case "socks5":
		d, err := xproxy.SOCKS5("tcp", cand.Addr(), nil, dialer)
		if err != nil {
			return nil, err
		}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if cd, ok := d.(xproxy.ContextDialer); ok {
				return cd.DialContext(ctx, network, addr)
			}
			return d.Dial(network, addr)
		}
	default:
		return nil, fmt.Errorf("unsupported proxy type %q", cand.Scheme)
	}
	return tr, nil
}

// checkCandidate measures how long cand needs to serve one of testURLs.
// The measurement starts from a direct connection to the proxy.
func checkCandidate(ctx context.Context, cand Candidate, testURLs []string, timeout time.Duration) (time.Duration, error) {
	tr, err := newTransport(&cand, timeout)
	if err != nil {
		return 0, err
	}
	defer tr.CloseIdleConnections()

	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	var lastErr error
	for _, target := range testURLs {
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "*/*")

		start := time.Now()
		resp, err := client.Do(req)
		elapsed := time.Since(start)
		if err != nil {
			lastErr = err
			cancel()
			continue
		}
		_, _ = io.CopyN(io.Discard, resp.Body, 4096)
		resp.Body.Close()
		cancel()
		if resp.StatusCode > 0 && resp.StatusCode < 400 {
			return elapsed, nil
		}
		lastErr = fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no test URL")
	}
	return 0, lastErr
}

// socks4Dialer implements the SOCKS4 (no authentication) handshake.
type socks4Dialer struct {
	proxyAddr string
	timeout   time.Duration
}

func (d *socks4Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" {
		return nil, fmt.Errorf("socks4: unsupported network %q", network)
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}

	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil {
		// SOCKS4 needs an IPv4 address, so resolve locally.
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, candidate := range ips {
			if v4 := candidate.IP.To4(); v4 != nil {
				ip = v4
				break
			}
		}
		if ip == nil {
			return nil, fmt.Errorf("socks4: no IPv4 address for %s", host)
		}
	}
	ip4 := ip.To4()

	dialer := &net.Dialer{Timeout: d.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", d.proxyAddr)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(d.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	request := []byte{0x04, 0x01, byte(port >> 8), byte(port), ip4[0], ip4[1], ip4[2], ip4[3], 0x00}
	if _, err := conn.Write(request); err != nil {
		conn.Close()
		return nil, err
	}
	reply := make([]byte, 8)
	if _, err := io.ReadFull(conn, reply); err != nil {
		conn.Close()
		return nil, err
	}
	if reply[0] != 0x00 || reply[1] != 0x5a {
		conn.Close()
		return nil, fmt.Errorf("socks4: request rejected (0x%02x)", reply[1])
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
