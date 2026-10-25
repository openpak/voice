# Notes: voice

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
| Media plane (SFU, mixing, codec bridging, ICE hand-out) | **not implemented, not registered**: no RPC pretends to move audio. See [`media-plane.md`](media-plane.md) |
| Postgres / persistence | none, on purpose: a session is as live as its participants' leases; a restart drops sessions and adapters re-create |
| WebSocket signaling for the phone app | not here; the website's `internal/voice` keeps serving it (migration proposal in the workspace doc) |

## Not yet enforced

Not yet enforced (listed so nobody assumes): a block created *after* someone
joined does not remove them until they rejoin; there is no voice-specific
privacy or voice-ban flag in the account core to consult.
