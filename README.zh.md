# proxyharvest

**语言：** [فارسی](README.md) · [English](README.en.md) · [Русский](README.ru.md) · [Español](README.es.md) · [中文](README.zh.md)

Go 命令行工具：从代码内置的网站注册表中收集免费代理，通过你的直连真实测试并按速度排序。

Linux 与 Windows（amd64/arm64）的静态二进制发布在 [Releases](https://github.com/FossilizedProgrammer/proxyharvest/releases) 页面 — 由 GitHub 通过 Actions 从本源码自动构建。

## 构建

要求：Go 1.27+。依赖极简（仅 `golang.org/x/net`），直接下载：

```bash
./build.sh          # 四个目标：linux/amd64+arm64, windows/amd64+arm64
# 或手动：
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o proxyharvest .
```

二进制为静态构建，运行时无需任何外部库。推送 `v*` 标签后 GitHub Actions 会自动构建并发布 Release。

## 使用

```bash
# 全部网站、全部国家，默认输出到 proxies.txt
./proxyharvest

# 仅美国代理，最快的 20 个
./proxyharvest -country US -top 20 -out us.txt

# 仅 HTTP 和 SOCKS5
./proxyharvest -types http,socks5

# 显示每个来源的详情（成功/失败、数量、备用来源）
./proxyharvest -v
```

`-country` 接受两位 ISO 国家代码（如 `US`、`DE`、`IR`）。

## 查看与选择网站

每个网站都有短名称。查看全部网站及其下载地址：

```bash
./proxyharvest -list-sources
```

用 `-sources` 只从选定的网站收集代理（逗号分隔）：

```bash
# 只用两个网站
./proxyharvest -sources spys.me,geonode.com

# 排除一个网站（前缀 ! 或 -）
./proxyharvest -sources '!spys.me'

# 只从一个网站取 SOCKS5
./proxyharvest -sources spys.me -types socks5
```

未知名称会报错并提示运行 `-list-sources`。日志中也会显示选中的网站数：

```
proxyharvest: collecting proxies from 2 of 11 sites: spys.me, geonode.com (country=all, types=http)
```

## 上游代理

如果到列表网站的直接访问被封，可用 `-upstream` 提供一个中间代理。它 **只** 用于下载网站列表（以及国家查询）；代理测试阶段始终使用直连，上游代理不参与任何测试。

```bash
# 带可选用户名/密码的 HTTP 代理
./proxyharvest -upstream http://user:pass@10.0.0.5:8080

# SOCKS5 以及简写 host:port（默认视为 HTTP）
./proxyharvest -upstream socks5://10.0.0.5:1080
./proxyharvest -upstream 10.0.0.5:1080
```

支持的协议：`http`、`https` 和 `socks5`（可选 `user:pass@` 认证）。端口必须显式给出。

## 真实站点可达性检查

通过速度测试并不代表该代理能在浏览器中使用：许多免费代理只应答简单请求，却放不开到大型网站的 TLS 隧道。因此速度测试之后，每个代理会通过自身打开 `https://www.facebook.com` 和 `https://www.youtube.com`（带有效证书的完整 HTTPS 请求，和浏览器一样），只有 **两个都成功** 的才保留。返回 5xx 或隧道/证书错误的会被剔除。

```
./proxyharvest                       # Facebook + YouTube，默认
./proxyharvest -require www.instagram.com,www.youtube.com
./proxyharvest -require none         # 关闭该检查
```

地址用逗号分隔；不给 scheme 时按 `https://` 处理。用 `-v` 可以看到每个代理被拒的原因。

> Firefox 对 HSTS 网站报 `SEC_ERROR_UNKNOWN_ISSUER` 说明该代理在解密 TLS 并出示伪造证书。HSTS 不允许例外，唯一办法是不用这个代理；`-require` 检查正是把这类代理标记为 `TLS interception` 而拒绝。如果不开代理也看到该错误，则是杀毒/防火墙或 ISP 网络在做 MITM，与本工具无关。

## 选项

| 选项 | 默认 | 说明 |
|---|---|---|
| `-country` | 全部 | 仅收集/测试该国家 |
| `-exclude-countries` | `IR,RU,CN,KP` | 完全不使用这些国家的代理（`none` — 关闭） |
| `-types` | `http,socks4,socks5` | 代理类型 |
| `-c` | `100` | 并行测试数 |
| `-timeout` | `8s` | 单个代理测试超时 |
| `-fetch-timeout` | `25s` | 每个来源的下载超时 |
| `-test-url` | `https://www.gstatic.com/generate_204` | 速度测试地址（自动尝试 HTTP 版本） |
| `-max-per-source` | `500` | 每个来源最多候选数 |
| `-top` | `0`（全部） | 只保留最快的 N 个 |
| `-out` | `proxies.txt` | 输出文件 |
| `-upstream` | — | 仅用于下载网站列表的上游代理（代理测试不经它） |
| `-no-retry` | — | 跳过不可达网站，不用收集到的代理去重试 |
| `-require` | `www.facebook.com,www.youtube.com` | 每个代理必须按 HTTPS 打开这些网站；`none` — 关闭 |
| `-sources` | 全部 | 仅从这些网站收集（名称见 `-list-sources`）；`!name` 排除某个网站 |
| `-list-sources` | — | 显示已知网站及其地址 |

## 进度显示

运行时在 `stdout` 上有一条实时状态行，显示当前阶段、计数与已用时间 — 工作耗时长时也能看出工具在忙碌而非卡死：

```
| downloading source lists: 7/11 requests, 2015 candidates [6s]
```

阶段依次为：`downloading source lists` → `checking harvested proxies` → `recovering unreachable sources` → `resolving proxy countries` → `testing proxies`。如果输出被重定向到文件，这些行每隔几秒写入 `stderr`，保持 `stdout` 干净。

## 工作原理

1. 域名来自 **代码内置的注册表** — 不需要任何外部文件。
2. 所有来源并行下载 — 直接下载（或经 `-upstream` 如果配置）。不可达的来源会用从其他来源收集到的可用代理重试；`-no-retry` 跳过该阶段。
3. 代理 **只从直连** 测试一次（没有任何中间代理参与测试）；仅建立经过被测代理自身的连接。
4. 死亡或不可达的代理被完全丢弃。
5. 来源中无法确定国家的代理，通过 `ip-api.com` 查询国家。然后应用 `-country` 过滤（单国家）与 `-exclude-countries` 黑名单（默认伊朗、俄罗斯、中国和朝鲜）；黑名单国家的代理即使通过测试也不出现在输出中。

```
./proxyharvest                                  # 剔除 IR, RU, CN, KP（默认）
./proxyharvest -exclude-countries IR,CN
./proxyharvest -exclude-countries none          # 允许所有国家
```
6. 输出按测得的延迟排序（升序）。

## 输出格式

```
# columns: proxy_url	country	latency_ms	source
http://1.2.3.4:8080	US	210	freeproxyhub.net
socks5://5.6.7.8:1080	DE	540	spys.me
```

免费代理不稳定，可能几分钟后就失效。使用风险自负。

## 许可证

MIT — 全文见 [LICENSE](LICENSE)。
