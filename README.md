# Voice Server

OpenPak's Voice Server.

| | |
| --- | --- |
| Network | Console-agnostic: platform adapters map their native flows onto it |
| Role | Voice sessions: who is in which session and who may join, over gRPC |
| Depends on | [`account`](https://github.com/openpak/account) (the account core: `CheckInteraction`, social and events contracts) |
| Image | `ghcr.io/openpak/voice` |
| Licence | AGPL-3.0-only |

## What it serves

| Endpoint | What |
| --- | --- |
| gRPC `openpak.voice.v1.Voice` | Sessions, participants, invitations, leases and a watch stream |
| HTTP `/healthz`, `/metrics` | Health and metrics |

## Run

```sh
GOWORK=off go test -race ./...
```

## Docs

- [Protocol](docs/protocol.md)
- [Evidence](docs/evidence.md)
- [Deploy](docs/deploy.md)
- [Notes](docs/notes.md)
- [Media plane](docs/media-plane.md)

## Licence

AGPL-3.0-only. See [LICENSE](LICENSE).
