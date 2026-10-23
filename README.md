# OpenPak voice

The neutral, console-agnostic voice session service. It owns **who is in which
voice session and who may join**: the account core's friendship, block and
account-status answer at the door (`CheckInteraction`, the same answer every
surface gets), invitations announced on the core's event stream, participant
leases, and the session lifecycle. It never speaks a console protocol: platform
adapters (`xb-voice` first; later PS3, Switch, Xbox 360 and the phone app's
signaling) translate their native flows onto its gRPC contract, so an Xbox
console and a phone user can be in the same session record.

Design and evidence: internal research notes (sections 1 and 5).

## What exists, what does not

| Part | State |
| --- | --- |
| Session / participant / invitation plane, authorization, leases, watch | built, unit and bufconn tested (v0.1.0) |
| Per-adapter keys bound to namespaces, native references (`xbox` -> XNKID) | built |
| Media plane (SFU, mixing, codec bridging, ICE hand-out) | **not implemented, not registered**: no RPC pretends to move audio. See `docs/media-plane.md` |
| Postgres / persistence | none, on purpose: a session is as live as its participants' leases; a restart drops sessions and adapters re-create |
| WebSocket signaling for the phone app | not here; the website's `internal/voice` keeps serving it (migration proposal in the workspace doc) |

## gRPC (`openpak.voice.v1.Voice`)

`CreateSession`, `GetSession`, `GetSessionByNativeRef`, `ListSessions`,
`JoinSession`, `Heartbeat`, `LeaveSession`, `EndSession`, `Kick`, `SetMuted`,
`Invite`, `SetNativeRef`, `WatchSession`. Comments in
`proto/openpak/voice/v1/voice.proto` are the contract.

Rules (all in `internal/session`):

- **Join** is allowed for the owner, for a friend of the owner (account core
  `CheckInteraction(joiner, owner, DISCOVER)`), or for an invitee, in every case
  unless the account core reports a block or an inactive account between the
  joiner and **any** present participant (both directions). If the account core
  cannot be asked the join is refused (fail closed). Denials surface as
  `PERMISSION_DENIED` whose message is the core's reason (`BLOCKED`,
  `NOT_FRIENDS`, `ACCOUNT_INACTIVE`, `SELF`).
- **Invite**: the actor is a participant and the core allows `INVITE`; recorded
  with a TTL (default 10 min) and announced as event `voice_invite`
  (`subject_id` = session id) so each family delivers it natively.
- **Owner** only: `EndSession`, `Kick`. `end_when_owner_leaves` ends the session
  when the owner goes (the Xbox host model); otherwise ownership passes to the
  longest-present participant.
- **Leases**: participants hold a lease (default 120 s, max 4 h) renewed by
  `Heartbeat`/`JoinSession`; adapters that cannot see a participant leave rely on
  expiry. Empty sessions end. Ended sessions stay readable for 5 minutes.
- **Namespaces**: each adapter key names a namespace (`xbox`, `app`, ...). An
  adapter only changes participants it admitted and sets native references in
  its own namespace; reads are open.
- Max 16 participants (the Xbox mesh ceiling), 8 live sessions per account.

Not yet enforced (listed so nobody assumes): a block created *after* someone
joined does not remove them until they rejoin; there is no voice-specific
privacy or voice-ban flag in the account core to consult.

## Adapters' tests

`github.com/openpak/voice/testkit` starts the real service in process with a stand-in account core (`testkit.New(t, testkit.XboxKey)`), the way the xb services expose a `testkit` to xb-e2e.

## Run

Ports: 20160 HTTP (`/healthz`, `/metrics`), 20161 gRPC, 20162 reserved.
Configuration is in `example.env`; secrets are files (`*_FILE`).

```sh
GOWORK=off go test -race ./...
```

## Repository

Module `github.com/openpak/voice`, image `ghcr.io/openpak/voice` (static, scratch,
uid 10001). Release by pushing a `vX.Y.Z` tag (`.github/workflows/release.yml`;
tags and manual dispatch only). The account protos in `internal/accountpb` are
the account core's `social` and `events` contracts, copied the way `chat` copies
them.
