# proxyharvest

**Languages:** [فارسی](README.md) · [Русский](README.ru.md) · [Español](README.es.md) · [中文](README.zh.md) · [English](README.en.md)

A Go command-line tool that harvests free proxies from the websites listed in the `sites` file, really tests them from your direct connection and sorts them by speed.

Static binaries for Linux and Windows (amd64/arm64) are published on the [Releases](https://github.com/FossilizedProgrammer/proxyharvest/releases) page — GitHub builds them via Actions from this same source.

## Building

Requirement: Go 1.27+. The dependency is minimal (`golang.org/x/net`) and is downloaded directly:

```bash
./build.sh          # all four targets: linux/amd64+arm64, windows/amd64+arm64
# or manually:
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o proxyharvest .
```

The binaries are static and need nothing at runtime. Releases are published automatically via GitHub Actions when you push a `v*` tag.

## Usage

```bash
# all sites, all countries, default output to proxies.txt
./proxyharvest

# US-only proxies, the 20 fastest
./proxyharvest -country US -top 20 -out us.txt

# HTTP and SOCKS5 only
./proxyharvest -types http,socks5

# show details per source (success/failure, counts, fallbacks)
./proxyharvest -v
```

`-country` takes a two-letter ISO code (e.g. `US`, `DE`, `IR`).

## Upstream proxy

If direct access to the listed sites is blocked, pass an intermediate proxy with `-upstream`. It is used **only** to download the site lists (and the country lookup); the testing phase still runs from your direct connection, and the upstream proxy is never in the path of any test.

```bash
# HTTP proxy with optional user/password
./proxyharvest -upstream http://user:pass@10.0.0.5:8080

# SOCKS5 and the short host:port form (defaults to HTTP)
./proxyharvest -upstream socks5://10.0.0.5:1080
./proxyharvest -upstream 10.0.0.5:1080
```

Accepted schemes: `http`, `https` and `socks5` (with optional `user:pass@` authentication). The port must be explicit.

## Real-site reachability check

Passing the speed test does not mean the proxy is usable in a browser: many free proxies answer a simple request but will not tunnel TLS to the big sites. That is why, after the speed test, every proxy opens `https://www.facebook.com` and `https://www.youtube.com` through itself (a full HTTPS request with a valid certificate, just like a browser) and only the ones that open **both** stay in the output. Proxies answering 5xx or a tunnel/certificate error are dropped.

```
./proxyharvest                       # Facebook + YouTube, default
./proxyharvest -require www.instagram.com,www.youtube.com
./proxyharvest -require none         # disable this check
```

Separate the addresses with commas; if the scheme is omitted `https://` is assumed. With `-v` the rejection reason of every proxy is logged.

> The `SEC_ERROR_UNKNOWN_ISSUER` error in Firefox on HSTS sites means the proxy is decrypting TLS and presenting a fake certificate. HSTS does not allow exceptions, so the only way out is not to use that proxy; the `-require` check rejects exactly those as `TLS interception`. If you see the error without any proxy, your antivirus/firewall or the ISP network is doing the MITM — it is not the tool's fault.

## Options

| Flag | Default | Meaning |
|---|---|---|
| `-sites` | `sites` | file with the site addresses (one domain per line) |
| `-country` | all | only that country |
| `-exclude-countries` | `IR,RU,CN,KP` | proxies from these countries are never used (`none` — disable) |
| `-types` | `http,socks4,socks5` | proxy types |
| `-c` | `100` | parallel proxy tests |
| `-timeout` | `8s` | timeout for each proxy test |
| `-fetch-timeout` | `25s` | timeout for downloading each source |
| `-test-url` | `https://www.gstatic.com/generate_204` | speed-test URL (the HTTP version is tried automatically) |
| `-max-per-source` | `500` | maximum candidates taken per site |
| `-top` | `0` (all) | keep only the N fastest |
| `-out` | `proxies.txt` | output file |
| `-upstream` | — | upstream proxy used only to download the site lists (tests never pass through it) |
| `-no-retry` | — | drop unreachable sources instead of retrying them through harvested proxies |
| `-require` | `www.facebook.com,www.youtube.com` | every proxy must open these sites over HTTPS; `none` — disable |
| `-list-sources` | — | print the known proxy-list sites |

## Progress display

While running, a live status line is printed on `stdout` with the current phase, counters and elapsed time — when the work takes long, you can see the tool is busy rather than hung:

```
| downloading source lists: 7/11 requests, 2015 candidates [6s]
```

Phases: `downloading source lists` → `checking harvested proxies` → `recovering unreachable sources` → `resolving proxy countries` → `testing proxies`. If you redirect the output to a file, the same lines go to `stderr` every few seconds, keeping `stdout` clean.

## How it works

1. The domains are read from the `sites` file. For unknown domains the main page is also checked with the generic `IP:PORT` extractor.
2. All sources are downloaded in parallel — directly (or through `-upstream` when set). An unreachable source is retried through the healthy proxies harvested from other sources; with `-no-retry` this step is skipped.
3. The proxies are tested once and **from the direct connection** (no intermediate proxy takes part in the test); the only connection established is through the proxy under test.
4. Dead and unreachable proxies are dropped completely.
5. For proxies whose country is unknown from the source, the country is looked up at `ip-api.com`. Then the `-country` filter (a single country) and the `-exclude-countries` blacklist (by default Iran, Russia, China and North Korea) are applied; blacklisted proxies never appear in the output even if they passed the tests.

```
./proxyharvest                                  # drop IR, RU, CN, KP (default)
./proxyharvest -exclude-countries IR,CN
./proxyharvest -exclude-countries none          # allow every country
```
6. The output is sorted by the measured latency (ascending).

## Output format

```
# columns: proxy_url	country	latency_ms	source
http://1.2.3.4:8080	US	210	freeproxyhub.net
socks5://5.6.7.8:1080	DE	540	spys.me
```

Free proxies are unstable and may die within minutes. You are responsible for using them.

## License

MIT — full text in [LICENSE](LICENSE).
