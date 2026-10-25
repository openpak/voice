# Deploy: voice

## Run

Ports: 20160 HTTP (`/healthz`, `/metrics`), 20161 gRPC, 20162 reserved.
Configuration is in [`example.env`](../example.env); secrets are files (`*_FILE`).

```sh
GOWORK=off go test -race ./...
```

## Repository

Module `github.com/openpak/voice`, image `ghcr.io/openpak/voice` (static, scratch,
uid 10001). Release by pushing a `vX.Y.Z` tag ([`.github/workflows/release.yml`](../.github/workflows/release.yml);
tags and manual dispatch only). The account core's `social` and `events` contracts
come from `github.com/openpak/account-api`.
