# Changelog — voice

`git log` stays the source of truth; this file is the readable summary.

## v0.1.1 — 2026-10-17

- `testkit`: the real service in process (bufconn) with a stand-in account core, for adapters' end-to-end tests.

## v0.1.0 — 2026-10-17

- First release: the session and authorization plane. In-memory sessions with
  participants, leases, invitations (announced as `voice_invite` on the account
  core's event stream), owner rules, native references and a watch stream.
- Authorization through the account core's `CheckInteraction`; fails closed.
- Per-adapter keys bound to namespaces. gRPC `openpak.voice.v1.Voice` on 20161,
  `/healthz` and `/metrics` on 20160.
- No media plane (see `docs/media-plane.md`).
