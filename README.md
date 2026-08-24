# filestor

Simple cookie-authenticated HTTP service built on Go `net/http`.

## Quick start

```bash
cp config.example.yml config.yml
# edit admin.username and admin.password
go run . -config config.yml -listen :8080
```

Open `http://127.0.0.1:8080`. Unauthenticated visits redirect to `/login`. After sign-in, a signed HttpOnly cookie keeps the session.

## Flags and environment

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| `-config` | `FILESTOR_CONFIG` | `config.yml` | YAML config path |
| `-listen` | `FILESTOR_LISTEN` | `:8080` | HTTP listen address |

## Config

```yaml
admin:
  username: admin
  password: REPLACE_ME
```

`admin.username` and `admin.password` are required. Do not commit `config.yml`.

## Auth

- Cookie name `filestor`, HMAC-SHA256 over `username|expiry`, HttpOnly, SameSite=Lax
- `Secure` is set when the request is TLS or `X-Forwarded-Proto: https`
- Session TTL is 12 hours; changing the admin password invalidates existing cookies
- Failed logins wait 1 second before responding
- `GET /healthz` is unauthenticated
