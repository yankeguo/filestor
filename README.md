# filestor

Cookie-authenticated browser for an Aliyun OSS bucket. Listing goes through this service; downloads redirect to a short-lived presigned URL so object bytes never transit filestor.

## Features

- Standard `net/http` server
- YAML config: `admin.{username,password}`, `aliyun.oss.{endpoint,bucket,access_key_id,access_key_secret}`, optional `upload.workspace`
- Cookie login (HMAC, HttpOnly, SameSite=Lax)
- Prefix listing treated as folders (`Delimiter=/`)
- `GET /download` 302s to a 5-minute OSS GET URL (`Content-Disposition: attachment`)
- Local upload workspace at `/upload` (list, drag-and-drop add, delete). Files stay on disk until pushed.
- One-click push of staged files to OSS under `YYYY/MM/YYYYMMDD-HHmm-TITLE/` (async job, one at a time, live progress on the page).
- SIGINT/SIGTERM stops accepting connections and waits for in-flight requests; a second signal terminates

## Quick start

```bash
cp config.example.yaml config.yaml
# edit admin credentials and aliyun.oss.*
go run . -config config.yaml -listen :8080
```

Or with Docker:

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/config.yaml:ro" \
  -v "$PWD/upload-workspace:/upload-workspace" \
  ghcr.io/yankeguo/filestor:latest
```

Open `http://127.0.0.1:8080`. Unauthenticated visits to `/browse` redirect to `/login`. After sign-in, `/` 302s to `/browse` so you can walk prefixes and download via OSS presigned URLs. `/` itself is unauthenticated and reserved for later pages; the navbar brand still points there.

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
aliyun:
  oss:
    endpoint: https://oss-cn-hangzhou.aliyuncs.com
    bucket: example-bucket
    access_key_id: REPLACE_ME
    access_key_secret: REPLACE_ME
upload:
  workspace: upload-workspace
```

All of `admin.username`, `admin.password`, and `aliyun.oss.{endpoint,bucket,access_key_id,access_key_secret}` are required. Field names follow the official Aliyun OSS API (`access_key_id` / `access_key_secret` / `endpoint`), not AWS/boto3 names. `endpoint` may omit `https://`; it is added on load. `upload.workspace` defaults to `upload-workspace` (relative to the process working directory; in the container that is `/upload-workspace`). Do not commit `config.yaml`.

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
| `GET` | `/browse?prefix=&marker=` | cookie | List current prefix (`Delimiter=/`, 200 keys per page) |
| `GET` | `/download?key=` | cookie | Sign a 5-minute GET URL and 302 to OSS |
| `GET` | `/upload` | cookie | Local workspace page (polls the directory every 1s) |
| `GET` | `/upload/files` | cookie | JSON list of regular files in the workspace |
| `POST` | `/upload/files` | cookie | Multipart field `file` (one or more); writes into the workspace |
| `DELETE` | `/upload/files?name=` | cookie | Delete one workspace file |
| `POST` | `/upload/push` | cookie | Form `time` (`YYYY-MM-DDTHH:mm`) + `title`; starts the async OSS push (409 while one is running) |
| `GET` | `/upload/push/status` | cookie | JSON progress of the current/last push job |

Folder rows are common prefixes; files skip the placeholder object whose key equals the current prefix. `/upload` only manages a local staging directory (flat files only: no folders, no hidden files starting with `.`; names are basenames). Pushing uploads every staged file to `YYYY/MM/YYYYMMDD-HHmm-TITLE/<name>` in the bucket — the picked time is used as-is regardless of timezone, and the title is sanitized to `[-_.A-Za-z0-9]` plus CJK letters with other chars folded to `-`. Each file is removed from staging once it lands; on failure the job stops and the remaining files stay staged. There is no search.

## Docker / GHCR

Images: `ghcr.io/yankeguo/filestor`

| Git event | Image tag |
|---|---|
| Push `main` | `latest` |
| Push a git tag | that tag (e.g. `v1.0.0`) |

Workflow: `.github/workflows/release.yml`.

## Development

```bash
go test ./...
```

Go 1.27+. `config.yaml` holds secrets and is gitignored.

## License

MIT, Y.-K. Guo
