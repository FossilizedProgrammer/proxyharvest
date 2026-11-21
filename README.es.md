# proxyharvest

**Idiomas:** [فارسی](README.md) · [English](README.en.md) · [Русский](README.ru.md) · [Español](README.es.md) · [中文](README.zh.md)

Herramienta de línea de comandos en Go para recopilar proxies gratuitos del registro interno de sitios (en el código), probarlos realmente desde tu conexión directa y ordenarlos por velocidad.

Binarios estáticos para Linux y Windows (amd64/arm64) publicados en la página de [Releases](https://github.com/FossilizedProgrammer/proxyharvest/releases) — el propio GitHub los compila mediante Actions desde este mismo código.

## Compilación

Requisito: Go 1.27+. La dependencia es mínima (`golang.org/x/net`) y se descarga directamente:

```bash
./build.sh          # los cuatro objetivos: linux/amd64+arm64, windows/amd64+arm64
# o manualmente:
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o proxyharvest .
```

Los binarios son estáticos y no requieren nada al ejecutarse. Las releases se publican automáticamente mediante GitHub Actions al hacer push de una etiqueta `v*`.

## Uso

```bash
# todos los sitios, todos los países, salida por defecto en proxies.txt
./proxyharvest

# solo proxies de EE.UU., los 20 más rápidos
./proxyharvest -country US -top 20 -out us.txt

# solo HTTP y SOCKS5
./proxyharvest -types http,socks5

# mostrar detalles de cada fuente (éxito/fallo, cantidad, alternativas)
./proxyharvest -v
```

`-country` acepta un código ISO de dos letras (por ejemplo `US`, `DE`, `IR`).

## Proxy upstream

Si el acceso directo a los sitios listados está bloqueado, pasa un proxy intermedio con `-upstream`. Se usa **solo** para descargar las listas de sitios (y la consulta de país); la fase de pruebas siempre se realiza desde tu conexión directa y el proxy upstream no participa en ninguna prueba.

```bash
# Proxy HTTP con usuario y contraseña opcionales
./proxyharvest -upstream http://user:pass@10.0.0.5:8080

# SOCKS5 y forma corta host:port (por defecto HTTP)
./proxyharvest -upstream socks5://10.0.0.5:1080
./proxyharvest -upstream 10.0.0.5:1080
```

Esquemas aceptados: `http`, `https` y `socks5` (con autenticación opcional `user:pass@`). El puerto debe ser explícito.

## Verificación de sitios reales

Pasar la prueba de velocidad no significa que el proxy sirva para el navegador: muchos proxies gratuitos responden a una petición simple pero no dejan pasar el túnel TLS hacia los grandes sitios. Por eso, tras la prueba de velocidad, cada proxy abre por sí mismo `https://www.facebook.com` y `https://www.youtube.com` (petición HTTPS completa con certificado válido, como un navegador) y solo los que abran **ambos** permanecen en la salida. Los que respondan 5xx o error de túnel/certificado se descartan.

```
./proxyharvest                       # Facebook y YouTube, por defecto
./proxyharvest -require www.instagram.com,www.youtube.com
./proxyharvest -require none         # sin esta verificación
```

Separa las direcciones con comas; si no indicas el esquema se asume `https://`. Con `-v` se registra el motivo del rechazo de cada proxy.

> El error `SEC_ERROR_UNKNOWN_ISSUER` en Firefox para sitios HSTS significa que el proxy está descifrando el TLS y muestra un certificado falso. Como HSTS no permite excepciones, la única salida es no usar ese proxy; la verificación `-require` los rechaza como `TLS interception`. Si el error aparece también sin proxy, el MITM lo hace el antivirus/firewall o la red del ISP — no es cosa de la herramienta.

## Opciones

| Flag | Predeterminado | Descripción |
|---|---|---|
| `-country` | todos | solo ese país |
| `-exclude-countries` | `IR,RU,CN,KP` | los proxies de estos países no se usan nunca (`none` — desactivar) |
| `-types` | `http,socks4,socks5` | tipos de proxy |
| `-c` | `100` | pruebas en paralelo |
| `-timeout` | `8s` | tiempo máximo por prueba de proxy |
| `-fetch-timeout` | `25s` | tiempo máximo de descarga de cada fuente |
| `-test-url` | `https://www.gstatic.com/generate_204` | URL de la prueba de velocidad (la versión HTTP se prueba automáticamente) |
| `-max-per-source` | `500` | máximo de candidatos por sitio |
| `-top` | `0` (todos) | conservar solo los N más rápidos |
| `-out` | `proxies.txt` | archivo de salida |
| `-upstream` | — | proxy upstream solo para descargar los sitios (las pruebas no pasan por él) |
| `-no-retry` | — | abandonar los sitios inaccesibles en vez de recuperarlos vía proxies |
| `-require` | `www.facebook.com,www.youtube.com` | cada proxy debe abrir estos sitios por HTTPS; `none` — desactivar |
| `-list-sources` | — | mostrar los sitios conocidos |

## Progreso

Durante la ejecución se muestra una línea de estado en vivo en `stdout` con la fase actual, el contador y el tiempo transcurrido — si el trabajo tarda, se ve claramente que la herramienta está ocupada y no colgada:

```
| downloading source lists: 7/11 requests, 2015 candidates [6s]
```

Fases: `downloading source lists` → `checking harvested proxies` → `recovering unreachable sources` → `resolving proxy countries` → `testing proxies`. Si rediriges la salida a un archivo, estas líneas van cada pocos segundos a `stderr` para dejar `stdout` limpio.

## Cómo funciona

1. Los dominios provienen del **registro interno en el código** — no se necesita ningún archivo externo.
2. Todas las fuentes se descargan en paralelo — directamente (o vía `-upstream` si está configurado). Un fuente inaccesible se reintenta a través de los proxies sanos recogidos de las otras fuentes; con `-no-retry` este paso se omite.
3. Los proxies se prueban **desde la conexión directa** (ningún proxy intermedio interviene); solo se establece la conexión a través del propio proxy bajo prueba.
4. Los proxies muertos o inalcanzables se descartan por completo.
5. Para los proxies cuyo país no esté claro en la fuente, el país se consulta a `ip-api.com`. Luego se aplican el filtro `-country` (un solo país) y la lista negra `-exclude-countries` (por defecto Irán, Rusia, China y Corea del Norte); los proxies de esos países no aparecen en la salida aunque pasen las pruebas.

```
./proxyharvest                                  # excluye IR, RU, CN, KP (por defecto)
./proxyharvest -exclude-countries IR,CN
./proxyharvest -exclude-countries none          # permitir todos los países
```
6. La salida se ordena por la latencia medida (ascendente).

## Formato de salida

```
# columns: proxy_url	country	latency_ms	source
http://1.2.3.4:8080	US	210	freeproxyhub.net
socks5://5.6.7.8:1080	DE	540	spys.me
```

Los proxies gratuitos son inestables y pueden morir a los pocos minutos. La responsabilidad de usarlos es tuya.

## Licencia

MIT — texto completo en [LICENSE](LICENSE).
