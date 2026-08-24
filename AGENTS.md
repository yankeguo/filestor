# Agent guide

Reply in the same language as the user.

filestor is a small Go (1.27+) cookie-authenticated browser for one Aliyun OSS bucket. Treat the code as source of truth. Keep changes focused; do not add frameworks or extra services unless asked. Style reference is **nanollm** (same author's sibling project): single `package main`, std `net/http`, `github.com/yankeguo/rg`, `gopkg.in/yaml.v3`, `github.com/stretchr/testify`, Bootstrap 5.3 dark + icons from jsdelivr with SRI.

## Layout

Single `package main`. No `internal/` split unless the tree clearly outgrows one package.

| File | Role |
|---|---|
| `main.go` | Flags, listen address, graceful shutdown with no deadline (`github.com/yankeguo/rg`) |
| `config.go` | YAML load/validate; OSS endpoint `https://` prefix if missing |
| `auth.go` | HMAC session cookie `filestor`; `requireAuth` 302s to `/login` |
| `server.go` | `net/http` mux, security headers, login/logout, `/` → `/browse`, browse + download handlers |
| `oss.go` | `ObjectStore` (`List`, `SignGetURL`); Aliyun OSS SDK v3 behind `ossStore` |
| `browse.go` | Prefix normalize, breadcrumbs, parent, table rows, size/time formatting |
| `upload.go` | Local upload workspace: list/add/delete under `upload.workspace`; `/upload` UI |
| `web/*.html` | Embedded HTML. Bootstrap 5.3 (jsdelivr CDN, SRI) with `data-bs-theme="dark"` + Bootstrap Icons; keep SRI hashes matching nanollm unless bumping the CDN URLs |
| `web_tmpl.go` | `//go:embed web/*.html` |

Tests live next to the code (`*_test.go`). Use `github.com/stretchr/testify`. Prefer extending an existing test file over adding a new one for the same area. HTTP tests inject `fakeStore`; do not call OSS.

## Hard constraints

- **std `net/http` only** for the server. No Fiber/Gin/Echo.
- Config shape is `admin.{username,password}`, `aliyun.oss.{endpoint,bucket,access_key_id,access_key_secret}` (all required), and optional `upload.workspace` (default `upload-workspace`). Names follow the **official Aliyun OSS API**, not AWS/boto3 (`secret_access_key`, `endpoint_url`, `region`). `endpoint` may omit `https://`; `normalizeOSSEndpoint` adds it on load.
- **Do not proxy object bytes.** `GET /download?key=` signs a GET URL (`signURLTTL` = 5 minutes, `ResponseContentDisposition(attachment)`) and 302s to OSS.
- Listing uses `ListObjects` with `Prefix`, `Delimiter("/")`, `Marker`, `MaxKeys(200)`. Skip the placeholder object whose key equals the current prefix.
- Browser lives at **`GET /browse`**. `GET /` is an unauthenticated 302 to `/browse` so `/` can host other pages later. Navbar brand stays `href="/"`. Login success and already-logged-in `GET /login` redirect to `/browse`. Browse links stay under `/browse?...`, never `/`.
- `/browse`, `/download`, and `/upload*` are cookie-protected. `GET /healthz` and `GET /` are not. Cookie: HMAC-SHA256 over `username|expiry`, HttpOnly, SameSite=Lax; `Secure` when TLS or `X-Forwarded-Proto: https`. TTL 12 hours; key is derived from username+password so a password change invalidates sessions. Failed login delays 1s. Constant-time compare via SHA256 of credentials.
- `http.Server.Shutdown` uses an unbounded context: SIGINT/SIGTERM stops accepting connections and waits indefinitely; after the first signal, SIGINT/SIGTERM is unregistered so a second signal can terminate. Do not add a Shutdown timeout.
- **`/upload` is a local staging directory only.** `GET /upload` + `GET|POST|DELETE /upload/files`. Write files under `upload.workspace` (create the dir if missing). List only regular files in the workspace root; skip subdirectories and dot-prefixed hidden files (incl. `.upload-*` temp files). Sanitize names with `path.Base`; reject empty names and any dot-prefixed (hidden) name, which covers `.`, `..`, and `.upload-*`. The UI also skips folder drops and hidden files client-side. Do not PUT to OSS from this page. Do not serve workspace bytes. Multipart field is `file`; max request size 2 GiB.
- There is no OSS upload, search, or multi-bucket UI unless asked.
- Do not commit `config.yaml` (secrets). Change `config.example.yaml` and README when the config schema changes.
- Images: `ghcr.io/${{ github.repository }}`. Push `main` → `latest`; push a git tag → that tag. Workflow: `.github/workflows/release.yml`. Image sets `FILESTOR_CONFIG=/config.yaml`. Flags/env: `-config`/`FILESTOR_CONFIG` default `config.yaml`; `-listen`/`FILESTOR_LISTEN` default `:8080`.

## Style

- Match nearby files: `log` package, table-driven tests, `httptest` for HTTP.
- `main` may use `rg.Must` / `rg.Guard`; library-ish functions return `error`.
- Keep YAML config the only user-facing knobs besides listen/config flags.
- Public docs and fixtures: placeholders (`example-bucket`, `REPLACE_ME`), never real secrets.
- Do not hit Aliyun in unit tests; inject `ObjectStore` (`fakeStore` in `browse_test.go`).

## Verify

```bash
go test ./...
gofmt -w .
```

After config, auth, browse, download, upload, or routing changes, update `README.md` (operators) and this file (agents) in the same change when behavior diverges from what they currently say.
