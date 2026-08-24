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
| `oss.go` | `ObjectStore` (`List`, `SignGetURL`, `Put`); Aliyun OSS SDK v3 behind `ossStore` |
| `browse.go` | Prefix normalize, breadcrumbs, parent, table rows, size/time formatting |
| `upload.go` | Local upload workspace: list/add/delete under `upload.workspace`; `/upload` UI |
| `events.go` | Shared SSE hub (`GET /upload/events`): lock, progress, files, state; workspace mutex |
| `push.go` | Async push of staged files to OSS under `YYYY/MM/YYYYMMDDhhmm-TITLE/`; single-flight via the hub lock |
| `suggest.go` | LLM title suggestion: chat-completions tool loop; `set_title` / `set_datetime` write `.upload-state.json` |
| `convert.go` | Suggest-time file conversion (`soffice` / ImageMagick / `pdftotext` / `pandoc`) in a temp dir; never mutates staging |
| `web/*.html` | Embedded HTML. Bootstrap 5.3 (jsdelivr CDN, SRI) with `data-bs-theme="dark"` + Bootstrap Icons; keep SRI hashes matching nanollm unless bumping the CDN URLs |
| `web_tmpl.go` | `//go:embed web/*.html` |

Tests live next to the code (`*_test.go`). Use `github.com/stretchr/testify`. Prefer extending an existing test file over adding a new one for the same area. HTTP tests inject `fakeStore`; do not call OSS.

## Hard constraints

- **std `net/http` only** for the server. No Fiber/Gin/Echo.
- Config shape is `admin.{username,password}`, `aliyun.oss.{endpoint,bucket,access_key_id,access_key_secret}` (all required), optional `upload.workspace` (default `upload-workspace`), and optional `llm.{url,model,effort,headers}` (OpenAI-compatible endpoint for `POST /upload/suggest`; `url` and `model` must be set together, `effort` is the model's reasoning effort, `headers` is a map of extra HTTP headers). Names follow the **official Aliyun OSS API**, not AWS/boto3 (`secret_access_key`, `endpoint_url`, `region`). `endpoint` may omit `https://`; `normalizeOSSEndpoint` adds it on load.
- **Do not proxy object bytes.** `GET /download?key=` signs a GET URL (`signURLTTL` = 5 minutes, `ResponseContentDisposition(attachment)`) and 302s to OSS.
- Listing uses `ListObjects` with `Prefix`, `Delimiter("/")`, `Marker`, `MaxKeys(200)`. Skip the placeholder object whose key equals the current prefix. If a page is truncated but `NextMarker` is empty, fall back to the last listed object key or common prefix.
- Browser lives at **`GET /browse`**. `GET /` is an unauthenticated 302 to `/browse` so `/` can host other pages later. Navbar brand stays `href="/"`. Login success and already-logged-in `GET /login` redirect to `/browse`. Browse links stay under `/browse?...`, never `/`.
- `/browse`, `/download`, and `/upload*` are cookie-protected. `GET /healthz` and `GET /` are not. Cookie: HMAC-SHA256 over `username|expiry`, HttpOnly, SameSite=Lax; `Secure` when TLS or `X-Forwarded-Proto: https`. TTL 12 hours; key is derived from username+password so a password change invalidates sessions. Failed login delays 1s. Constant-time compare via SHA256 of credentials.
- `http.Server.Shutdown` uses an unbounded context: SIGINT/SIGTERM stops accepting connections and waits indefinitely; after the first signal, SIGINT/SIGTERM is unregistered so a second signal can terminate. Do not add a Shutdown timeout.
- **`/upload` is a local staging directory plus an OSS push.** `GET /upload` + `GET|POST|DELETE /upload/files` manage staging: write files under `upload.workspace` (create the dir if missing), list only regular files in the workspace root, skip subdirectories and dot-prefixed hidden files (incl. `.upload-*` temp files). Sanitize names with `path.Base`; reject empty names and any dot-prefixed (hidden) name, which covers `.`, `..`, and `.upload-*`. The UI also skips folder drops and hidden files client-side, pre-checks the 2 GiB per-file limit, and stages files sequentially (one XHR `POST` per file) so it can show real per-file/overall byte progress; a failed file does not abort the rest of the batch. Do not serve workspace bytes. Multipart field is `file`; max request size 2 GiB. The first staged file pins the draft push options (`time` = now, `title` = "") in `.upload-state.json` as soon as that file is saved; `PUT /upload/state` (form `time` + `title`, called by the page on input change) updates them while files are staged (no-op on an empty workspace). The page prefills both inputs from this state, so a reload keeps the pinned time. The Suggest button is omitted unless `llm.url` is set. The state file is removed when staging empties — all files deleted or a successful push that leaves nothing behind (files added on disk during a running push stay staged and keep the state). Staging add/delete, suggest, and push share one exclusive workspace lock (`stage` / `suggest` / `push`); a second mutation gets 409. `PUT /upload/state` is 409 while suggest or push holds the lock. `GET /upload/events` (SSE) pushes `snapshot` / `lock` / `files` / `state` / `progress` / `done` / `error`; the page uses EventSource instead of polling. A subscriber whose event buffer fills is disconnected, so its EventSource reconnects to a fresh snapshot instead of silently missing events; acquiring a lock clears the previous job so stale results never render under a new operation. Staging byte progress stays on XHR `upload.onprogress`.
- **LLM title suggestion is an async tool loop.** `POST /upload/suggest` (503 unless `llm.url` is set, 400 on empty staging, 409 if the workspace is locked) lists the staged files, acquires the `suggest` lock, returns 202, and runs up to 8 rounds of chat completions in a background goroutine against `llm.url` with std `net/http` (`reasoning_effort` from `llm.effort`, extra headers from `llm.headers`). Progress and the chosen title/time go over SSE (errors are not echoed from the upstream LLM). Four tools: `read_file_as_text` (sanitized workspace name; native text is NUL-sniffed and capped at 64 KiB; office/PDF binaries are converted in a temp dir via `pdftotext`/`soffice`/`pandoc`/`catdoc` and never mutate staging), `read_file_as_image` (jpeg/png/gif only to the model, 8 MiB cap; oversized or other formats are converted via ImageMagick, with `soffice --convert-to png` as a fallback), `set_datetime` (optional; writes only `Time` into `.upload-state.json` when the model finds a clear document date, `YYYY-MM-DD` or `YYYY-MM-DDTHH:mm`, keeping the title), `set_title` (writes only `Title`, keeping the pinned time; required to finish). Tool-call `arguments` may be a JSON object or a JSON string containing one. Conversion CLIs are installed in the container image; local `go run` still serves native text and small jpeg/png/gif.
- **Push to OSS is async and single-flight.** `POST /upload/push` (form `time` = `YYYY-MM-DDTHH:mm` wall clock, `title`) snapshots the staged files, acquires the `push` lock, and starts a background job that `Put`s each to `YYYY/MM/YYYYMMDDhhmm-TITLE/<name>` (title sanitized: letters/digits/`_`/`.`, everything else folds to `-`, max 80 runes) and removes it from staging on success; the first failure stops the job and keeps the rest staged. Only one workspace job runs at a time — a second `POST` gets 409. Progress is broadcast on `GET /upload/events` (byte updates throttled ~10Hz). The job goroutine is not joined on shutdown.
- There is no search or multi-bucket UI unless asked.
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
