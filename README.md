# URL Shortener

[![CI](https://github.com/umer-78/url-shortener-go/actions/workflows/ci.yml/badge.svg)](https://github.com/umer-78/url-shortener-go/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.25%2B-00add8)
![Static binary](https://img.shields.io/badge/binary-static%2C%20no%20cgo-blue)
![License](https://img.shields.io/badge/license-MIT-green)

A URL shortener in Go: JSON API, redirects, visit counting, custom codes,
expiring links and a small web page — one static binary with an embedded page
and a pure-Go SQLite driver, so deployment is a single file.

```bash
$ go run ./cmd/shortener
listening on :8080, serving links as http://localhost:8080/<code>

$ curl -s -X POST localhost:8080/api/links -H 'content-type: application/json' \
       -d '{"url":"https://github.com/umer-78","code":"me","expires_in":"720h"}'
{"code":"me","short_url":"http://localhost:8080/me","target":"https://github.com/umer-78",
 "created_at":"2026-09-17T18:17:57Z","expires_at":"2026-10-17T18:17:57Z","visits":0}

$ curl -s -o /dev/null -w '%{http_code} -> %{redirect_url}\n' localhost:8080/me
302 -> https://github.com/umer-78
```

## API

| Method | Path | Notes |
|---|---|---|
| `POST` | `/api/links` | `{"url": "...", "code": "optional", "expires_in": "24h"}` |
| `GET` | `/api/links` | most recent links (`?limit=`) |
| `GET` | `/api/links/{code}` | one link, without counting a visit |
| `DELETE` | `/api/links/{code}` | |
| `GET` | `/api/stats` | links, total visits, expired |
| `GET` | `/{code}` | 302 to the target, counting the visit |
| `GET` | `/health` · `/` | health check · the page |

Status codes: `201` created, `409` code taken, `422` bad URL, bad code or bad
duration, `429` rate limited, `404` unknown code, `410` expired.

## Decisions worth explaining

- **Base62 minus look-alikes.** `0/O` and `1/l/I` are left out, so a code read
  over the phone or copied from a screenshot still works.
- **`crypto/rand`, not `math/rand`.** Codes should not be predictable; guessable
  codes let anyone enumerate other people's links.
- **302, not 301.** A permanent redirect is cached by the browser for ever, and
  after that the visit counter never sees the hit again.
- **The counter is one SQL statement.** `visits = visits + 1` in the database
  instead of read-modify-write in Go: 50 concurrent hits count 50, which a test
  asserts.
- **Codes grow under pressure.** Generated codes start at six characters and get
  longer after repeated collisions, rather than retrying for ever.
- **Expired links are purged hourly**, so the table does not fill with links
  nobody can follow.
- **Rate limited per client** (30 new links a minute) to stop one script filling
  the database. In production this belongs at the edge as well.

## Run it

```bash
git clone https://github.com/umer-78/url-shortener-go.git
cd url-shortener-go
go test ./...
go run ./cmd/shortener -addr :8080 -db links.db -base-url https://s.example.com
```

Flags read from the environment too: `ADDR`, `DB`, `BASE_URL`.

### Docker

```bash
docker build -t url-shortener .
docker run -p 8080:8080 -v url-data:/data url-shortener
```

The image is `distroless/static` with a non-root user: no shell, no package
manager, nothing but the binary.

## Tests

```bash
go test -race -cover ./...
```

Covering URL normalisation and rejection of `javascript:` and non-web schemes,
the code alphabet, custom-code validation and collisions, expiry and purging,
list/delete/stats, every handler and status code, the rate limiter, and two
concurrency tests: 50 simultaneous visits all counted, and 40 simultaneous
creates all getting different codes.

## Not included

Authentication (anyone who can reach it can create links), per-link analytics
beyond a counter, and abuse checking of targets. Behind a public address, put it
behind auth and a safe-browsing check.

## License

[MIT](LICENSE)
