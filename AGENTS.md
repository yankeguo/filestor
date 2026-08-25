# Agent guide

Reply in the same language as the user.

filestor is a small Go (1.27+) cookie-authenticated browser for one S3-compatible bucket (Aliyun OSS, Qcloud COS, AWS S3, MinIO, …). Treat the code as source of truth. Keep changes focused; do not add frameworks or extra services unless asked. Style reference is **nanollm** (same author's sibling project): single `package main`, std `net/http`, `github.com/yankeguo/rg`, `gopkg.in/yaml.v3`, `github.com/stretchr/testify`, Bootstrap 5.3 dark + icons from jsdelivr with SRI.

## Layout

Single `package main`. No `internal/` split unless the tree clearly outgrows one package.

| File | Role |
|---|---|
| `main.go` | Flags, listen address, graceful shutdown with no deadline (`github.com/yankeguo/rg`) |
| `config.go` | YAML load/validate; S3 endpoint `https://` prefix if missing |
| `auth.go` | HMAC session cookie `filestor`; `requireAuth` 302s to `/login` |
| `server.go` | `net/http` mux, security headers, login/logout, `/` → `/browse`, browse + download handlers |
| `s3.go` | `ObjectStore` (`List`, `SignGetURL`, `Put`); `aws-sdk-go-v2` behind `s3Store` (checksums only when required, explicit `ContentLength`) |
| `browse.go` | Calendar browse over the fixed `YYYY/MM/` layout, prefix contents view: breadcrumbs, parent, table rows, size/time formatting |
| `upload.go` | Local upload workspace: list/add/delete under `upload.workspace`; `/upload` UI; `.filestor/` meta dir (`state.json`, `tmp/`, `cache/`) and startup `prepWorkspace` (clean stale temp files, prune cache) |
| `events.go` | Shared SSE hub (`GET /upload/events`): lock, progress, files, state; workspace mutex |
| `push.go` | Async push of staged files to the bucket under `YYYY/MM/YYYYMMDDhhmm-TITLE/`; single-flight via the hub lock |
| `suggest.go` | LLM title suggestion: chat-completions tool loop; `set_title` / `set_datetime` write `.filestor/state.json` |
| `convert.go` | Suggest-time file conversion (`soffice` / ImageMagick / `pdftotext` / `pandoc`); results cached by source SHA-256 under `.filestor/cache` (7-day TTL); never mutates staging |
| `templates/*.html` | Embedded HTML. Bootstrap 5.3 (jsdelivr CDN, SRI) with `data-bs-theme="dark"` + Bootstrap Icons; keep SRI hashes matching nanollm unless bumping the CDN URLs |
| `web_tmpl.go` | `//go:embed templates/*.html` |

Tests live next to the code (`*_test.go`). Use `github.com/stretchr/testify`. Prefer extending an existing test file over adding a new one for the same area. HTTP tests inject `fakeStore`; never hit a real object store.

## Hard constraints

- **std `net/http` only** for the server. No Fiber/Gin/Echo.
- Config shape is `admin.{username,password}`, `s3.{endpoint,region,bucket,access_key_id,secret_access_key}` (all required), optional `s3.force_path_style` (MinIO-style vendors), optional `upload.workspace` (default `upload-workspace`), and optional `llm.{url,model,effort,headers}` (OpenAI-compatible endpoint for `POST /upload/suggest`; `url` and `model` must be set together, `effort` is the model's reasoning effort, `headers` is a map of extra HTTP headers). Names follow the **S3 multi-vendor convention** (`secret_access_key`, `region`, `force_path_style` — as in litestream/rclone), not vendor-specific APIs. `endpoint` may omit `https://`; `normalizeEndpoint` adds it on load. Aliyun OSS needs its S3-compatible endpoint (`https://s3.oss-{region}.aliyuncs.com`).
- **Do not proxy object bytes.** `GET /download?key=` signs a GET URL (`signURLTTL` = 5 minutes, `ResponseContentDisposition(attachment)`) and 302s to the bucket.
- Listing uses `ListObjectsV2` with `Prefix`, `Delimiter("/")`, `StartAfter`, `MaxKeys(200)`. Skip the placeholder object whose key equals the current prefix. If a page is truncated, resume from the last listed object key or common prefix (`StartAfter`; no `ContinuationToken` plumbing).
- Browser lives at **`GET /browse`**. `GET /` is an unauthenticated 302 to `/browse` so `/` can host other pages later. Navbar brand stays `href="/"`. Login success and already-logged-in `GET /login` redirect to `/browse`. Browse links stay under `/browse?...`, never `/`. With no params, `/browse` renders a **calendar** for the current month (weeks start Monday) over the fixed `YYYY/MM/YYYYMMDDhhmm-TITLE/` layout: the month's common prefixes are collected with bounded pagination (10 pages), days holding a `YYYYMMDD…` directory are highlighted, and landing selects today. `?month=YYYY-MM` switches months, `&day=YYYY-MM-DD` selects a day (out-of-month days are dropped) and lists that day's directories — parsed into `hh:mm` + title — on the right; clicking one opens `?prefix=`, the classic contents view (breadcrumbs, `..` row, pagination). A day given without a valid month pulls its own month into view.
- `/browse`, `/download`, and `/upload*` are cookie-protected. `GET /healthz` and `GET /` are not. Cookie: HMAC-SHA256 over `username|expiry`, HttpOnly, SameSite=Lax; `Secure` when TLS or `X-Forwarded-Proto: https`. TTL 12 hours; key is derived from username+password so a password change invalidates sessions. Failed login delays 1s. Constant-time compare via SHA256 of credentials.
- `http.Server.Shutdown` uses an unbounded context: SIGINT/SIGTERM stops accepting connections and waits indefinitely; after the first signal, SIGINT/SIGTERM is unregistered so a second signal can terminate. Do not add a Shutdown timeout.
- **`/upload` is a local staging directory plus a bucket push.** `GET /upload` + `GET|POST|DELETE /upload/files` manage staging: write files under `upload.workspace` (create the dir if missing), list only regular files in the workspace root, skip subdirectories and dot-prefixed hidden files (incl. the `.filestor/` meta directory). Sanitize names with `path.Base`; reject empty names and any dot-prefixed (hidden) name, which covers `.`, `..`, and `.filestor`. The UI also skips folder drops and hidden files client-side, pre-checks the 2 GiB per-file limit, and stages files sequentially (one XHR `POST` per file) so it can show real per-file/overall byte progress; a failed file does not abort the rest of the batch. Do not serve workspace bytes. Multipart field is `file`; max request size 2 GiB. The first staged file pins the draft push options (`time` = now, `title` = "") in `.filestor/state.json` as soon as that file is saved; `PUT /upload/state` (form `time` + `title`, called by the page on input change) updates them while files are staged (no-op on an empty workspace). The page prefills both inputs from this state, so a reload keeps the pinned time. The Suggest button is omitted unless `llm.url` is set. The state file is removed when staging empties — all files deleted or a successful push that leaves nothing behind (files added on disk during a running push stay staged and keep the state). All non-staged files live under the workspace's hidden `.filestor/` directory: `state.json`, `tmp/` (atomic-write temp files for uploads and state), and `cache/` (conversion results). `prepWorkspace` (called by `NewServer`) creates it, removes stale temp files, and prunes the cache. Staging add/delete, suggest, and push share one exclusive workspace lock (`stage` / `suggest` / `push`); a second mutation gets 409. `PUT /upload/state` is 409 while suggest or push holds the lock. `GET /upload/events` (SSE) pushes `snapshot` / `lock` / `files` / `state` / `progress` / `done` / `error`; the page uses EventSource instead of polling. A subscriber whose event buffer fills is disconnected, so its EventSource reconnects to a fresh snapshot instead of silently missing events; acquiring a lock clears the previous job so stale results never render under a new operation. Staging byte progress stays on XHR `upload.onprogress`.
- **LLM title suggestion is an async tool loop.** `POST /upload/suggest` (503 unless `llm.url` is set, 400 on empty staging, 409 if the workspace is locked) lists the staged files, acquires the `suggest` lock, returns 202, and runs up to 16 rounds / 24 tool calls of chat completions in a background goroutine against `llm.url` with std `net/http` (`reasoning_effort` from `llm.effort`, extra headers from `llm.headers`). The initial user message includes a one-line `peek:` at each native-text file's first 1 KiB (NUL-sniffed, whitespace-collapsed; binaries skipped) so most batches need no read calls. When the budget runs out the loop does not fail: reading is closed and one forced round offers only `set_title`/`set_datetime`, accepting a sanitized plain-text answer as a last resort. Progress and the chosen title/time go over SSE (errors are not echoed from the upstream LLM). Four tools: `read_file_as_text` (sanitized workspace name; native text is NUL-sniffed and capped at 64 KiB; office/PDF binaries are converted via `pdftotext`/`soffice`/`pandoc`/`catdoc` and never mutate staging), `read_file_as_image` (jpeg/png/gif only to the model, 8 MiB cap; oversized or other formats are converted via ImageMagick, with `soffice --convert-to png` as a fallback), `set_datetime` (optional; writes only `Time` into `.filestor/state.json` when the model finds a clear document date, `YYYY-MM-DD` or `YYYY-MM-DDTHH:mm`, keeping the title), `set_title` (writes only `Title`, keeping the pinned time; required to finish). Conversion results are memoized under `.filestor/cache/<sha256-of-content>.{txt,jpg}` (7-day TTL, pruned on startup and after each store), so re-reading an already-converted file skips the external converters. Tool-call `arguments` may be a JSON object or a JSON string containing one. Conversion CLIs are installed in the container image; local `go run` still serves native text and small jpeg/png/gif.
- **Push to the bucket is async and single-flight.** `POST /upload/push` (form `time` = `YYYY-MM-DDTHH:mm` wall clock, `title`) snapshots the staged files, acquires the `push` lock, and starts a background job that `Put`s each to `YYYY/MM/YYYYMMDDhhmm-TITLE/<name>` (title sanitized: letters/digits/`_`/`.`, everything else folds to `-`, max 80 runes) and removes it from staging on success; the first failure stops the job and keeps the rest staged. Only one workspace job runs at a time — a second `POST` gets 409. Progress is broadcast on `GET /upload/events` (byte updates throttled ~10Hz). The job goroutine is not joined on shutdown.
- There is no search or multi-bucket UI unless asked.
- Do not commit `config.yaml` (secrets). Change `config.example.yaml` and README when the config schema changes.
- Images: `ghcr.io/${{ github.repository }}`. Push `main` → `latest`; push a git tag → that tag. Workflow: `.github/workflows/release.yml`. Image sets `FILESTOR_CONFIG=/config.yaml`. Flags/env: `-config`/`FILESTOR_CONFIG` default `config.yaml`; `-listen`/`FILESTOR_LISTEN` default `:8080`.

## Style

- Match nearby files: `log` package, table-driven tests, `httptest` for HTTP.
- `main` may use `rg.Must` / `rg.Guard`; library-ish functions return `error`.
- Keep YAML config the only user-facing knobs besides listen/config flags.
- Public docs and fixtures: placeholders (`example-bucket`, `REPLACE_ME`), never real secrets.
- Do not hit a real object store in unit tests; inject `ObjectStore` (`fakeStore` in `browse_test.go`).

## Verify

```bash
go test ./...
gofmt -w .
```

After config, auth, browse, download, upload, or routing changes, update `README.md` (operators) and this file (agents) in the same change when behavior diverges from what they currently say.
