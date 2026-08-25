# filestor

Cookie-authenticated browser for one S3-compatible bucket (Aliyun OSS, Qcloud COS, AWS S3, MinIO, …). Listing goes through this service; downloads redirect to a short-lived presigned URL so object bytes never transit filestor.

## Features

- Standard `net/http` server
- YAML config: `admin.{username,password}`, `s3.{endpoint,region,bucket,access_key_id,secret_access_key}` (+ optional `s3.force_path_style`), optional `upload.workspace` and `llm.{url,model,effort,headers}`
- Cookie login (HMAC, HttpOnly, SameSite=Lax)
- Calendar browse at `/browse`: monthly grid (weeks start Monday) with days holding records highlighted; pick a day to list its record directories, click through to browse contents
- `GET /download` 302s to a 5-minute presigned GET URL (`Content-Disposition: attachment`)
- Local upload workspace at `/upload` (list, drag-and-drop add with per-file progress, delete). Files stay on disk until pushed. Staging, title suggestion, and bucket push share a workspace lock and live progress over `GET /upload/events`.
- One-click push of staged files to the bucket under `YYYY/MM/YYYYMMDDhhmm-TITLE/` (async job, one at a time, live progress on the page).
- One-click LLM title suggestion for staged files (async OpenAI-compatible tool loop: `read_file_as_text`, `read_file_as_image`, `set_datetime`, `set_title`). The initial prompt peeks at each text file's first 1 KiB so most batches need no read calls; if the read budget runs out, the model is forced to decide with read tools closed instead of failing. Office/PDF and odd or oversized images are converted at suggest-time inside the container.
- SIGINT/SIGTERM stops accepting connections and waits for in-flight requests; a second signal terminates

## Quick start

```bash
cp config.example.yaml config.yaml
# edit admin credentials and s3.*
go run . -config config.yaml -listen :8080
```

Or with Docker:

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/config.yaml:ro" \
  -v "$PWD/upload-workspace:/upload-workspace" \
  ghcr.io/yankeguo/filestor:latest
```

Open `http://127.0.0.1:8080`. Unauthenticated visits to `/browse` redirect to `/login`. After sign-in, `/` 302s to `/browse` so you can walk prefixes and download via presigned URLs. `/` itself is unauthenticated and reserved for later pages; the navbar brand still points there.

## Flags and environment

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-config` | `FILESTOR_CONFIG` | `config.yaml` | YAML config path |
| `-listen` | `FILESTOR_LISTEN` | `:8080` | HTTP listen address |

The container image sets `FILESTOR_CONFIG=/config.yaml`.

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

All of `admin.username`, `admin.password`, and `s3.{endpoint,region,bucket,access_key_id,secret_access_key}` are required; `s3.force_path_style` is optional (needed by MinIO-style vendors). Any S3-compatible storage works — Aliyun OSS (use its S3-compatible endpoint `https://s3.oss-{region}.aliyuncs.com`), Qcloud COS (`https://cos.{region}.myqcloud.com`), AWS S3, MinIO, etc. `endpoint` may omit `https://`; it is added on load. `upload.workspace` defaults to `upload-workspace` (relative to the process working directory; in the container that is `/upload-workspace`). `llm.{url,model,effort,headers}` is an optional OpenAI-compatible endpoint used by title suggestion on `/upload`; `url` and `model` must be set together, `effort` is the model's reasoning effort, and `headers` is an optional map of extra HTTP headers (e.g. `Authorization`). Do not commit `config.yaml`.

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
| `GET` | `/browse` | cookie | Calendar of the current month over `YYYY/MM/…`; days with records highlighted, today selected |
| `GET` | `/browse?month=YYYY-MM&day=YYYY-MM-DD` | cookie | Calendar for that month plus the day's record directories |
| `GET` | `/browse?prefix=&marker=` | cookie | Contents view for one prefix (`Delimiter=/`, 200 keys per page) |
| `GET` | `/download?key=` | cookie | Sign a 5-minute GET URL and 302 to the bucket |
| `GET` | `/upload` | cookie | Local workspace page (EventSource `/upload/events`) |
| `GET` | `/upload/events` | cookie | SSE: `snapshot`, `lock`, `files`, `state`, `progress`, `done`, `error`; lagging subscribers are dropped so EventSource reconnects to a fresh snapshot |
| `GET` | `/upload/files` | cookie | JSON list of regular files in the workspace |
| `POST` | `/upload/files` | cookie | Multipart field `file` (one or more); writes into the workspace (409 if locked) |
| `DELETE` | `/upload/files?name=` | cookie | Delete one workspace file (409 if locked) |
| `PUT` | `/upload/state` | cookie | Form `time` + `title`; persists the draft push options (no-op while nothing is staged; 409 during suggest/push) |
| `POST` | `/upload/suggest` | cookie | Start async LLM title suggestion (202; 409 if locked); result over SSE |
| `POST` | `/upload/push` | cookie | Form `time` (`YYYY-MM-DDTHH:mm`) + `title`; starts the async bucket push (409 if locked) |

The calendar assumes the fixed layout pushed by `/upload` (`YYYY/MM/YYYYMMDDhhmm-TITLE/`): directories are counted per day from the first 8 digits of their name and shown as `hh:mm` + title; non-conforming names are ignored by the calendar but still reachable from the contents view. Folder rows there are common prefixes; files skip the placeholder object whose key equals the current prefix. `/upload` only manages a local staging directory (flat files only: no folders, no hidden files starting with `.`; names are basenames). Pushing uploads every staged file to `YYYY/MM/YYYYMMDDhhmm-TITLE/<name>` in the bucket — the picked time is used as-is regardless of timezone, and the title is sanitized to `[-_.A-Za-z0-9]` plus CJK letters with other chars folded to `-`. Each file is removed from staging once it lands; on failure the job stops and the remaining files stay staged. The first staged file pins the push datetime (and any edited time/title) in `.upload-state.json` inside the workspace, so a page reload keeps them; the state is cleared when the staging area empties (all deleted, or a successful push with nothing left). The Suggest button is shown only when `llm.url` is configured. There is no search.

## Docker / GHCR

Images: `ghcr.io/yankeguo/filestor`

| Git event | Image tag |
|---|---|
| Push `main` | `latest` |
| Push a git tag | that tag (e.g. `v1.0.0`) |

Workflow: `.github/workflows/release.yml`. The image includes LibreOffice, ImageMagick, poppler-utils, pandoc, catdoc, and Noto CJK fonts so title suggestion can convert office/PDF/images at read time without changing staged files. Local `go run` still reads native text and small jpeg/png/gif without those tools.

## Development

```bash
go test ./...
```

Go 1.27+. `config.yaml` holds secrets and is gitignored.

## License

MIT, Y.-K. Guo
