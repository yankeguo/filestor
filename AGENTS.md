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
| `push.go` | Async push of staged files to OSS under `YYYY/MM/YYYYMMDDhhmm-TITLE/`; one job at a time, progress state |
| `suggest.go` | LLM title suggestion: chat-completions tool loop over workspace files; `set_title` writes `.upload-state.json` |
| `web/*.html` | Embedded HTML. Bootstrap 5.3 (jsdelivr CDN, SRI) with `data-bs-theme="dark"` + Bootstrap Icons; keep SRI hashes matching nanollm unless bumping the CDN URLs |
| `web_tmpl.go` | `//go:embed web/*.html` |

Tests live next to the code (`*_test.go`). Use `github.com/stretchr/testify`. Prefer extending an existing test file over adding a new one for the same area. HTTP tests inject `fakeStore`; do not call OSS.

## Hard constraints

- **std `net/http` only** for the server. No Fiber/Gin/Echo.
- Config shape is `admin.{username,password}`, `aliyun.oss.{endpoint,bucket,access_key_id,access_key_secret}` (all required), optional `upload.workspace` (default `upload-workspace`), and optional `llm.{url,model,effort,headers}` (OpenAI-compatible endpoint reserved for future LLM calls; `url` and `model` must be set together, `effort` is the model's reasoning effort, `headers` is a map of extra HTTP headers). Names follow the **official Aliyun OSS API**, not AWS/boto3 (`secret_access_key`, `endpoint_url`, `region`). `endpoint` may omit `https://`; `normalizeOSSEndpoint` adds it on load.
- **Do not proxy object bytes.** `GET /download?key=` signs a GET URL (`signURLTTL` = 5 minutes, `ResponseContentDisposition(attachment)`) and 302s to OSS.
- Listing uses `ListObjects` with `Prefix`, `Delimiter("/")`, `Marker`, `MaxKeys(200)`. Skip the placeholder object whose key equals the current prefix.
- Browser lives at **`GET /browse`**. `GET /` is an unauthenticated 302 to `/browse` so `/` can host other pages later. Navbar brand stays `href="/"`. Login success and already-logged-in `GET /login` redirect to `/browse`. Browse links stay under `/browse?...`, never `/`.
- `/browse`, `/download`, and `/upload*` are cookie-protected. `GET /healthz` and `GET /` are not. Cookie: HMAC-SHA256 over `username|expiry`, HttpOnly, SameSite=Lax; `Secure` when TLS or `X-Forwarded-Proto: https`. TTL 12 hours; key is derived from username+password so a password change invalidates sessions. Failed login delays 1s. Constant-time compare via SHA256 of credentials.
- `http.Server.Shutdown` uses an unbounded context: SIGINT/SIGTERM stops accepting connections and waits indefinitely; after the first signal, SIGINT/SIGTERM is unregistered so a second signal can terminate. Do not add a Shutdown timeout.
- **`/upload` is a local staging directory plus an OSS push.** `GET /upload` + `GET|POST|DELETE /upload/files` manage staging: write files under `upload.workspace` (create the dir if missing), list only regular files in the workspace root, skip subdirectories and dot-prefixed hidden files (incl. `.upload-*` temp files). Sanitize names with `path.Base`; reject empty names and any dot-prefixed (hidden) name, which covers `.`, `..`, and `.upload-*`. The UI also skips folder drops and hidden files client-side, pre-checks the 2 GiB per-file limit, and stages files sequentially (one XHR `POST` per file) so it can show real per-file/overall byte progress; a failed file does not abort the rest of the batch. Do not serve workspace bytes. Multipart field is `file`; max request size 2 GiB. The first staged file pins the draft push options (`time` = now, `title` = "") in `.upload-state.json` in the workspace; `PUT /upload/state` (form `time` + `title`, called by the page on input change) updates them while files are staged (no-op on an empty workspace). The page prefills both inputs from this state, so a reload keeps the pinned time. The state file is removed when staging empties — all files deleted or a successful push.
- **LLM title suggestion is a synchronous tool loop.** `POST /upload/suggest` (503 unless `llm.url` is set, 400 on empty staging) lists the staged files in the prompt and runs up to 8 rounds of chat completions against `llm.url` with std `net/http` (`reasoning_effort` from `llm.effort`, extra headers from `llm.headers`). Three tools: `read_text_file` (sanitized workspace name, NUL-sniffed, 64 KiB cap), `read_image_file` (png/jpg/jpeg/gif/webp, 8 MiB cap, sent back as a base64 `image_url` user part), `set_title` (writes only `Title` into `.upload-state.json`, keeping the pinned time). The handler returns the chosen title; the page fills the title input with it.
- **Push to OSS is async and single-flight.** `POST /upload/push` (form `time` = `YYYY-MM-DDTHH:mm` wall clock, `title`) snapshots the staged files and starts a background job that `Put`s each to `YYYY/MM/YYYYMMDDhhmm-TITLE/<name>` (title sanitized: letters/digits/`_`/`.`, everything else folds to `-`, max 80 runes) and removes it from staging on success; the first failure stops the job and keeps the rest staged. Only one job runs at a time — a second `POST` gets 409. `GET /upload/push/status` returns the mutex-guarded `pushState` (running, prefix, done/total files, done/total bytes, current file, error); the `/upload` page polls it every 1s for the progress bar. The job goroutine is not joined on shutdown.
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
