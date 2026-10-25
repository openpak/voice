# Protocol: voice

## gRPC (`openpak.voice.v1.Voice`)

`CreateSession`, `GetSession`, `GetSessionByNativeRef`, `ListSessions`,
`JoinSession`, `Heartbeat`, `LeaveSession`, `EndSession`, `Kick`, `SetMuted`,
`Invite`, `SetNativeRef`, `WatchSession`. Comments in
[`proto/openpak/voice/v1/voice.proto`](../proto/openpak/voice/v1/voice.proto) are the contract.

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

## Adapters' tests

`github.com/openpak/voice/testkit` starts the real service in process with a stand-in account core (`testkit.New(t, testkit.XboxKey)`), the way the xb services expose a `testkit` to xb-e2e.

## Account core contracts

The account core's `social` and `events` contracts come from `github.com/openpak/account-api`.
