# filestor

Cookie-authenticated browser for an Aliyun OSS bucket. Listing goes through this service; downloads redirect to a short-lived presigned URL so object bytes never transit filestor.

## Features

- Standard `net/http` server
- YAML config: `admin.{username,password}` and `aliyun.oss.{endpoint,bucket,access_key_id,access_key_secret}`
- Cookie login (HMAC, HttpOnly, SameSite=Lax)
- Prefix listing treated as folders (`Delimiter=/`)
- `GET /download` 302s to a 5-minute OSS GET URL (`Content-Disposition: attachment`)
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
```

All of `admin.username`, `admin.password`, and `aliyun.oss.{endpoint,bucket,access_key_id,access_key_secret}` are required. Field names follow the official Aliyun OSS API (`access_key_id` / `access_key_secret` / `endpoint`), not AWS/boto3 names. `endpoint` may omit `https://`; it is added on load. Do not commit `config.yaml`.

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

Folder rows are common prefixes; files skip the placeholder object whose key equals the current prefix. There is no upload, delete, or search.

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
