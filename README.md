# filestor

Cookie-authenticated browser for an Aliyun OSS bucket. Listing goes through this service; downloads redirect to a short-lived presigned URL so object bytes never transit filestor.

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

Open `http://127.0.0.1:8080`. Unauthenticated visits redirect to `/login`. After sign-in, `/` sends you to `/browse` to walk prefixes like folders and download via OSS presigned URLs.

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

All of `admin.username`, `admin.password`, and `aliyun.oss.{endpoint,bucket,access_key_id,access_key_secret}` are required. `endpoint` may omit `https://`; it is added on load. Do not commit `config.yaml`.

## Auth

- Cookie name `filestor`, HMAC-SHA256 over `username|expiry`, HttpOnly, SameSite=Lax
- `Secure` is set when the request is TLS or `X-Forwarded-Proto: https`
- Session TTL is 12 hours; changing the admin password invalidates existing cookies
- Failed logins wait 1 second before responding
- `GET /healthz` is unauthenticated

## Browse and download

- `GET /` redirects to `/browse`
- `GET /browse?prefix=&marker=` lists the current prefix (`Delimiter=/`, 200 keys per page)
- `GET /download?key=` signs a 5-minute GET URL and 302s to OSS
- Folder rows are common prefixes; files skip the placeholder object equal to the current prefix

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
