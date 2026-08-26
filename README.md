# filestor

Cookie-authenticated browser for one S3-compatible bucket (Aliyun OSS, Qcloud COS, AWS S3, MinIO, …). Listing goes through this service; downloads redirect to a short-lived presigned URL so object bytes never transit filestor.

## Features

- Standard `net/http` server
- YAML config: `admin.{username,password}`, `s3.{endpoint,region,bucket,access_key_id,secret_access_key}` (+ optional `s3.force_path_style`), optional `upload.workspace` and `llm.{url,model,effort,headers}`
- Cookie login (HMAC, HttpOnly, SameSite=Lax)
- Calendar browse at `/browse`: monthly grid (weeks start Monday, sticky on scroll) with days holding bundles highlighted, next to a list of every bundle day of the year (newest first, `#day-YYYY-MM-DD` anchors, scrolled to the selected day); click a bundle through to a dedicated bundle view (title/date header, file stats, image gallery and video/audio players via inline presigned URLs)
- `GET /download` 302s to a 5-minute presigned GET URL (`Content-Disposition: attachment`); `GET /preview` signs without it for inline rendering
- Local upload workspace at `/upload` (list, drag-and-drop add with per-file progress, delete). Files stay on disk until pushed. Staging, LLM analysis, and bucket push share a workspace lock and live progress over `GET /upload/events`.
- One-click push of staged files to the bucket as a bundle under `content/<aa>/<bb>/<uuid>/` (async job, one at a time, live progress on the page), with `.meta.json` and a monthly index entry at `index/YYYY/YYYY-MM.json`.
- One-click LLM analysis for staged files (async OpenAI-compatible tool loop: `read_file_as_text`, `read_file_as_image`, `rename_file`, `set_datetime`, `set_title`). The initial prompt peeks at each text file's first 1 KiB so most batches need no read calls; if the read budget runs out, the model is forced to decide with read tools closed instead of failing. Office/PDF and odd or oversized images are converted at analyze-time inside the container. The model may also rename clearly misnamed staged files (never overwriting another file); a push then uses the new names. While the LLM is configured, staged files must pass one successful analysis before they can be pushed; adding or deleting files resets that and disables the upload button until the next analysis.
- SIGINT/SIGTERM stops accepting connections and waits for in-flight requests; a second signal terminates

## Quick start

```bash
cp config.example.yaml config.yaml
# edit admin credentials and s3.*
(cd static && bun install && bun run build)  # build the embedded frontend assets
go run . -config config.yaml -listen :8080
```

Or with Docker:

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/config.yaml:ro" \
  -v "$PWD/upload-workspace:/upload-workspace" \
  ghcr.io/yankeguo/filestor:latest
```

Open `http://127.0.0.1:8080`. Unauthenticated visits to `/browse` redirect to `/login`. After sign-in, `/` 302s to `/browse` so you can browse bundles on the calendar and download via presigned URLs. `/` itself is unauthenticated and reserved for later pages; the navbar brand still points there.

## Flags and environment

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-config` | `FILESTOR_CONFIG` | `config.yaml` | YAML config path |
| `-listen` | `FILESTOR_LISTEN` | `:8080` | HTTP listen address |

The container image sets `FILESTOR_CONFIG=/config.yaml`.

## Frontend build

Page JS lives in `static/` as a bun + TypeScript project: entries under `static/src/entries/` are bundled by `bun run build` (via `Bun.build`) into self-contained IIFE files in `static/dist/`, named `<entry>-<content-hash>.js`. `static/dist` is embedded into the binary (`go:embed`) and served at `GET /static/` with immutable caching; templates resolve the hashed name with the `{{jsAsset "entry"}}` helper, so only the entry name is referenced in HTML. During development use `bun run dev` (watch mode with inline sourcemaps); `bun run typecheck` runs `tsc --noEmit`. The Docker image builds the bundles in a bun stage.

## Config

```yaml
admin:
  username: admin
  password: REPLACE_ME
s3:
  endpoint: https://s3.oss-cn-hangzhou.aliyuncs.com
  region: cn-hangzhou
  bucket: example-bucket
  access_key_id: REPLACE_ME
  secret_access_key: REPLACE_ME
  # force_path_style: false
upload:
  workspace: upload-workspace
# llm:
#   url: https://api.example.com/v1/chat/completions
#   model: my-model
#   effort: medium
#   headers:
#     Authorization: Bearer REPLACE_ME
```

All of `admin.username`, `admin.password`, and `s3.{endpoint,region,bucket,access_key_id,secret_access_key}` are required; `s3.force_path_style` is optional (needed by MinIO-style vendors). Any S3-compatible storage works — Aliyun OSS (use its S3-compatible endpoint `https://s3.oss-{region}.aliyuncs.com`), Qcloud COS (`https://cos.{region}.myqcloud.com`), AWS S3, MinIO, etc. `endpoint` may omit `https://`; it is added on load. `upload.workspace` defaults to `upload-workspace` (relative to the process working directory; in the container that is `/upload-workspace`). `llm.{url,model,effort,headers}` is an optional OpenAI-compatible endpoint used by file analysis on `/upload`; `url` and `model` must be set together, `effort` is the model's reasoning effort, and `headers` is an optional map of extra HTTP headers (e.g. `Authorization`). Do not commit `config.yaml`.

## Auth

- Cookie name `filestor`, HMAC-SHA256 over `username|expiry`, HttpOnly, SameSite=Lax
- `Secure` is set when the request is TLS or `X-Forwarded-Proto: https`
- Session TTL is 12 hours; changing the admin password invalidates existing cookies
- Failed logins wait 1 second before responding
- `GET /healthz` and `GET /` are unauthenticated

## HTTP

| Method | Path | Auth | Role |
|---|---|---|---|
| `GET` | `/healthz` | no | `OK` |
| `GET` | `/` | no | 302 to `/browse` |
| `GET`/`POST` | `/login` | no | Sign-in; already logged in → `/browse` |
| `POST` | `/logout` | cookie | Clear session, 302 to `/login` |
| `GET` | `/browse` | cookie | Calendar of the current month from the in-memory monthly indexes (loaded from `index/YYYY/YYYY-MM.json` at startup); days with bundles highlighted, today selected |
| `GET` | `/browse?month=YYYY-MM&day=YYYY-MM-DD` | cookie | Calendar for that month plus the year's bundle days (newest first, anchored and scrolled to the selected day) |
| `GET` | `/bundle/{id}` | cookie | Dedicated bundle view by UUID (header from the in-memory index with `.meta.json` fallback, stats, media preview, file list); 404 when unknown |
| `GET` | `/download?key=` | cookie | Sign a 5-minute GET URL (`Content-Disposition: attachment`) and 302 to the bucket |
| `GET` | `/preview?key=` | cookie | Sign a 5-minute GET URL without attachment disposition (inline render) and 302 to the bucket |
| `GET` | `/static/` | cookie | Embedded frontend assets (content-hashed names, immutable cache) |
| `GET` | `/upload` | cookie | Local workspace page (EventSource `/upload/events`) |
| `GET` | `/upload/events` | cookie | SSE: `snapshot`, `lock`, `files`, `state`, `progress`, `done`, `error`; lagging subscribers are dropped so EventSource reconnects to a fresh snapshot |
| `GET` | `/upload/files` | cookie | JSON list of regular files in the workspace |
| `POST` | `/upload/files` | cookie | Multipart field `file` (one or more); writes into the workspace (409 if locked) |
| `DELETE` | `/upload/files?name=` | cookie | Delete one workspace file (409 if locked) |
| `PUT` | `/upload/state` | cookie | Form `time` + `title`; persists the draft push options (no-op while nothing is staged; 409 during analyze/push) |
| `POST` | `/upload/analyze` | cookie | Start async LLM analysis (202; 409 if locked); result over SSE; a successful run flags the staged files as analyzed |
| `POST` | `/upload/push` | cookie | Form `time` (`YYYY-MM-DDTHH:mm`) + `title`; starts the async bucket push (409 if locked; 400 while the LLM is configured and the staged files are not analyzed) |

A **bundle** is one pushed batch of files: a UUID v4 directory at `content/<id[:2]>/<id[2:4]>/<uuid>/` with a `.meta.json` (`id`, `title`, `time`) and a matching entry in the monthly index `index/YYYY/YYYY-MM.json`. filestor is single-instance, so all monthly indexes are loaded into memory at startup (an unreadable or corrupt month file is logged and skipped) and kept up to date by pushes — the calendar is served from memory without touching the bucket: bundles are grouped by the `time` wall clock and shown as `hh:mm` + title. The right pane lists every bundle day of the displayed month's year, newest days first with `day-YYYY-MM-DD` anchors; landing selects today and scrolls it into view, and a selected day without bundles shows a hint instead. Opening a bundle goes to the dedicated `GET /bundle/{id}` page: a header with the title and date/time from the in-memory index (falling back to `.meta.json`; unknown ids are a 404, invalid ids a 400) plus a back link to the day, file count and total size (`.meta.json` is hidden), type-aware icons, and inline previews for browser-native media (image gallery, `<video>`/`<audio>` players over `/preview` presigned URLs; images above 32 MiB stay download-only). `/upload` only manages a local staging directory (flat files only: no folders, no hidden files starting with `.`; names are basenames). Pushing uploads `.meta.json` then every staged file to `content/<aa>/<bb>/<uuid>/<name>`, then rewrites that month's index file with the new entry appended and updates the in-memory copy once the write lands — the picked time is used as-is regardless of timezone, and the title is sanitized to `[-_.A-Za-z0-9]` plus CJK letters with other chars folded to `-`. Staged files are removed only after the index write lands; on failure the job stops (without updating the index) and everything stays staged for a retry. The first staged file pins the push datetime (and any edited time/title) in `.filestor/state.json` inside the workspace, so a page reload keeps them; the state is cleared when the staging area empties (all deleted, or a successful push with nothing left). All non-staged files live under the workspace's hidden `.filestor/` directory: `state.json`, a `tmp/` area for atomic writes, and a `cache/` of read-time conversions keyed by the source file's SHA-256 (re-reading an already-converted file skips the external converters; entries expire after 7 days and stale temp files are cleaned on startup). The Analyze button is shown only when `llm.url` is configured, and while it is configured the staged files must go through one successful analysis before pushing: a successful run sets `analyzed` in `.filestor/state.json`, adding or deleting staged files resets it, and the upload button stays disabled (push returns 400) until the next analysis. There is no search.

## Docker / GHCR

Images: `ghcr.io/yankeguo/filestor`

| Git event | Image tag |
|---|---|
| Push `main` | `latest` |
| Push a git tag | that tag (e.g. `v1.0.0`) |

Workflow: `.github/workflows/release.yml`. The image includes LibreOffice, ImageMagick, poppler-utils, pandoc, catdoc, and Noto CJK fonts so file analysis can convert office/PDF/images at read time without changing staged files. Local `go run` still reads native text and small jpeg/png/gif without those tools.

## Development

```bash
go test ./...
```

Go 1.27+. `config.yaml` holds secrets and is gitignored.

## License

MIT, Y.-K. Guo
