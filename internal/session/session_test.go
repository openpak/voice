package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/openpak/voice/internal/core"
	voicev1 "github.com/openpak/voice/proto/openpak/voice/v1"
)

// fakeCore answers like the account core: friends may interact, a block or an
// inactive account is a hard refusal, anyone else is NOT_FRIENDS.
type fakeCore struct {
	mu       sync.Mutex
	friends  map[[2]string]bool
	blocks   map[[2]string]bool // [blocker, blocked]
	inactive map[string]bool
	down     bool
	events   []string
}

func newCore() *fakeCore {
	return &fakeCore{friends: map[[2]string]bool{}, blocks: map[[2]string]bool{}, inactive: map[string]bool{}}
}
func (f *fakeCore) befriend(a, b string) {
	f.friends[[2]string{a, b}], f.friends[[2]string{b, a}] = true, true
}

func (f *fakeCore) Check(_ context.Context, actor, target string, _ core.Kind) (core.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.down:
		return core.Decision{}, errors.New("core down")
	case actor == target:
		return core.Decision{Reason: core.ReasonSelf}, nil
	case f.inactive[actor] || f.inactive[target]:
		return core.Decision{Reason: core.ReasonInactive}, nil
	case f.blocks[[2]string{actor, target}] || f.blocks[[2]string{target, actor}]:
		return core.Decision{Reason: core.ReasonBlocked}, nil
	case f.friends[[2]string{actor, target}]:
		return core.Decision{Allowed: true}, nil
	}
	return core.Decision{Reason: core.ReasonNotFriends}, nil
}

func (f *fakeCore) Announce(_ context.Context, typ, to, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, typ+":"+to+":"+id)
	return nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func setup(t *testing.T) (*Manager, *fakeCore, *clock) {
	t.Helper()
	fc := newCore()
	ck := &clock{t: time.Unix(1_700_000_000, 0)}
	return &Manager{Auth: fc, Announce: fc, Now: ck.now}, fc, ck
}

func mk(t *testing.T, m *Manager, owner string, endOnOwner bool) *voicev1.Session {
	t.Helper()
	s, _, err := m.Create(CreateParams{Adapter: "xbox", Owner: owner, Kind: voicev1.SessionKind_SESSION_KIND_NATIVE, EndOnOwnerLeft: endOnOwner})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func join(m *Manager, id, acct string) error {
	_, _, err := m.Join(context.Background(), JoinParams{Adapter: "xbox", SessionID: id, Account: acct})
	return err
}

func TestCreateIdempotentAndNativeRef(t *testing.T) {
	m, _, _ := setup(t)
	p := CreateParams{Adapter: "xbox", Owner: "a", IdempotencyKey: "k", NativeRef: "0011223344556677"}
	s, dup, err := m.Create(p)
	if err != nil || dup || len(s.Participants) != 1 || s.Participants[0].AccountId != "a" {
		t.Fatalf("create: %v %v %v", s, dup, err)
	}
	if s.MaxParticipants != MaxParticipants || s.NativeRefs["xbox"] != "0011223344556677" {
		t.Fatalf("defaults: %v", s)
	}
	again, dup, err := m.Create(p)
	if err != nil || !dup || again.Id != s.Id {
		t.Fatalf("idempotent: %v %v %v", again, dup, err)
	}
	if _, _, err := m.Create(CreateParams{Adapter: "xbox", Owner: "b", NativeRef: "0011223344556677"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("same native ref twice: %v", err)
	}
	got, err := m.ByNativeRef("xbox", "0011223344556677")
	if err != nil || got.Id != s.Id {
		t.Fatalf("by native ref: %v %v", got, err)
	}
	if _, err := m.ByNativeRef("app", "0011223344556677"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other namespace must not resolve: %v", err)
	}
}

func TestJoinRules(t *testing.T) {
	m, fc, _ := setup(t)
	fc.befriend("a", "b")
	fc.befriend("a", "c")
	s := mk(t, m, "a", false)

	if err := join(m, s.Id, "b"); err != nil {
		t.Fatalf("friend of the owner: %v", err)
	}
	var d DeniedError
	if err := join(m, s.Id, "stranger"); !errors.As(err, &d) || d.Reason != core.ReasonNotFriends {
		t.Fatalf("stranger: %v", err)
	}

	// b blocks c: c is a friend of the owner but cannot sit with b.
	fc.blocks[[2]string{"b", "c"}] = true
	if err := join(m, s.Id, "c"); !errors.As(err, &d) || d.Reason != core.ReasonBlocked {
		t.Fatalf("blocked by a participant: %v", err)
	}
	// And the reverse direction.
	delete(fc.blocks, [2]string{"b", "c"})
	fc.blocks[[2]string{"c", "b"}] = true
	if err := join(m, s.Id, "c"); !errors.As(err, &d) || d.Reason != core.ReasonBlocked {
		t.Fatalf("blocking a participant: %v", err)
	}
	delete(fc.blocks, [2]string{"c", "b"})

	// A stranger with an invitation gets in, unless blocked or inactive.
	fc.befriend("b", "x")
	if _, err := m.Invite(context.Background(), s.Id, "b", "x", 0); err != nil {
		t.Fatal(err)
	}
	if err := join(m, s.Id, "x"); err != nil {
		t.Fatalf("invited friend of a participant: %v", err)
	}
	fc.befriend("b", "y")
	if _, err := m.Invite(context.Background(), s.Id, "b", "y", 0); err != nil {
		t.Fatal(err)
	}
	fc.inactive["y"] = true
	if err := join(m, s.Id, "y"); !errors.As(err, &d) || d.Reason != core.ReasonInactive {
		t.Fatalf("invited but banned: %v", err)
	}
}

func TestJoinFailsClosedWhenCoreIsDown(t *testing.T) {
	m, fc, _ := setup(t)
	fc.befriend("a", "b")
	s := mk(t, m, "a", false)
	fc.down = true
	if err := join(m, s.Id, "b"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want unavailable, got %v", err)
	}
	got, _ := m.Get(s.Id)
	if len(got.Participants) != 1 {
		t.Fatal("a refused join must not admit")
	}
}

func TestFullRenewAndForeign(t *testing.T) {
	m, fc, _ := setup(t)
	s, _, _ := m.Create(CreateParams{Adapter: "xbox", Owner: "a", Max: 2})
	fc.befriend("a", "b")
	fc.befriend("a", "c")
	if err := join(m, s.Id, "b"); err != nil {
		t.Fatal(err)
	}
	if err := join(m, s.Id, "c"); !errors.Is(err, ErrFull) {
		t.Fatalf("full: %v", err)
	}
	_, renewed, err := m.Join(context.Background(), JoinParams{Adapter: "xbox", SessionID: s.Id, Account: "b"})
	if err != nil || !renewed {
		t.Fatalf("renew: %v %v", renewed, err)
	}
	if _, _, err := m.Join(context.Background(), JoinParams{Adapter: "app", SessionID: s.Id, Account: "b"}); !errors.Is(err, ErrForeign) {
		t.Fatalf("another adapter must not take over a participant: %v", err)
	}
	if err := m.Leave("app", s.Id, "b"); !errors.Is(err, ErrForeign) {
		t.Fatalf("leave by another adapter: %v", err)
	}
}

func TestOwnerLeaves(t *testing.T) {
	m, fc, _ := setup(t)
	fc.befriend("a", "b")
	end := mk(t, m, "a", true)
	_ = join(m, end.Id, "b")
	if err := m.Leave("xbox", end.Id, "a"); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(end.Id)
	if got.EndedUnix == 0 || len(got.Participants) != 0 {
		t.Fatalf("host leaving must end a host-bound session: %v", got)
	}
	if _, err := m.ByNativeRef("xbox", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	keep := mk(t, m, "a", false)
	_ = join(m, keep.Id, "b")
	if err := m.Leave("xbox", keep.Id, "a"); err != nil {
		t.Fatal(err)
	}
	got, _ = m.Get(keep.Id)
	if got.EndedUnix != 0 || got.OwnerAccount != "b" {
		t.Fatalf("ownership must pass on: %v", got)
	}
	_ = m.Leave("xbox", keep.Id, "b")
	got, _ = m.Get(keep.Id)
	if got.EndedUnix == 0 {
		t.Fatal("an empty session ends")
	}
}

func TestLeaseExpiry(t *testing.T) {
	m, fc, ck := setup(t)
	fc.befriend("a", "b")
	s := mk(t, m, "a", false)
	_, _, _ = m.Join(context.Background(), JoinParams{Adapter: "xbox", SessionID: s.Id, Account: "b", LeaseSec: 10})
	_, _ = m.Heartbeat(context.Background(), "xbox", s.Id, "a", 600)
	ck.t = ck.t.Add(11 * time.Second)
	m.Reap()
	got, _ := m.Get(s.Id)
	if len(got.Participants) != 1 || got.Participants[0].AccountId != "a" {
		t.Fatalf("b's lease ran out, a's did not: %v", got)
	}
	ck.t = ck.t.Add(time.Hour)
	m.Reap()
	got, _ = m.Get(s.Id)
	if got.EndedUnix == 0 {
		t.Fatal("everyone's lease ran out: the session ends")
	}
	ck.t = ck.t.Add(endedKeep + time.Second)
	m.Reap()
	if _, err := m.Get(s.Id); !errors.Is(err, ErrNotFound) {
		t.Fatal("ended sessions are forgotten after a while")
	}
}

func TestInviteAnnouncesOnce(t *testing.T) {
	m, fc, _ := setup(t)
	fc.befriend("a", "b")
	s := mk(t, m, "a", false)
	if _, err := m.Invite(context.Background(), s.Id, "stranger", "b", 0); !errors.Is(err, ErrNotParticipant) {
		t.Fatalf("only participants invite: %v", err)
	}
	var d DeniedError
	if _, err := m.Invite(context.Background(), s.Id, "a", "nobody", 0); !errors.As(err, &d) {
		t.Fatalf("the core refuses: %v", err)
	}
	dup, err := m.Invite(context.Background(), s.Id, "a", "b", 0)
	if err != nil || dup {
		t.Fatal(err, dup)
	}
	dup, err = m.Invite(context.Background(), s.Id, "a", "b", 0)
	if err != nil || !dup {
		t.Fatal(err, dup)
	}
	if len(fc.events) != 1 || fc.events[0] != "voice_invite:b:"+s.Id {
		t.Fatalf("events: %v", fc.events)
	}
}

func TestOwnerOnlyAndWatch(t *testing.T) {
	m, fc, _ := setup(t)
	fc.befriend("a", "b")
	s := mk(t, m, "a", false)
	_ = join(m, s.Id, "b")
	ch, cancel, err := m.Watch(s.Id)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if ev := <-ch; ev.Type != voicev1.SessionEvent_TYPE_SNAPSHOT || len(ev.Session.Participants) != 2 {
		t.Fatalf("snapshot: %v", ev)
	}
	if err := m.Kick(s.Id, "b", "a"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("non-owner kick: %v", err)
	}
	if err := m.End(s.Id, "b"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("non-owner end: %v", err)
	}
	if err := m.SetMuted("xbox", s.Id, "b", true); err != nil {
		t.Fatal(err)
	}
	if ev := <-ch; ev.Type != voicev1.SessionEvent_TYPE_MUTED || ev.AccountId != "b" {
		t.Fatalf("mute event: %v", ev)
	}
	if err := m.Kick(s.Id, "a", "b"); err != nil {
		t.Fatal(err)
	}
	if ev := <-ch; ev.Type != voicev1.SessionEvent_TYPE_LEFT || ev.Reason != "kicked" {
		t.Fatalf("kick event: %v", ev)
	}
	if err := m.End(s.Id, "a"); err != nil {
		t.Fatal(err)
	}
	if ev := <-ch; ev.Type != voicev1.SessionEvent_TYPE_ENDED {
		t.Fatalf("end event: %v", ev)
	}
	if _, open := <-ch; open {
		t.Fatal("the stream closes when the session ends")
	}
}

func TestHeartbeatEvictsOnBlock(t *testing.T) {
	m, fc, _ := setup(t)
	fc.befriend("a", "b")
	s := mk(t, m, "a", false)
	if err := join(m, s.Id, "b"); err != nil {
		t.Fatalf("join: %v", err)
	}
	// Mid-call block: the next heartbeat ends b's stay.
	fc.blocks[[2]string{"a", "b"}] = true
	if _, err := m.Heartbeat(context.Background(), "xbox", s.Id, "b", 600); err == nil {
		t.Fatal("blocked participant renewed")
	}
	got, _ := m.Get(s.Id)
	for _, p := range got.Participants {
		if p.AccountId == "b" {
			t.Fatal("blocked participant still listed")
		}
	}
}

func TestHeartbeatSurvivesCoreOutage(t *testing.T) {
	m, fc, _ := setup(t)
	fc.befriend("a", "b")
	s := mk(t, m, "a", false)
	if err := join(m, s.Id, "b"); err != nil {
		t.Fatalf("join: %v", err)
	}
	// An unanswerable core keeps the lease: renewals must survive a blip.
	fc.down = true
	if _, err := m.Heartbeat(context.Background(), "xbox", s.Id, "b", 600); err != nil {
		t.Fatalf("outage dropped the lease: %v", err)
	}
}

func TestHeartbeatKeepsUnfriendedButUnblocked(t *testing.T) {
	m, fc, _ := setup(t)
	fc.befriend("a", "b")
	s := mk(t, m, "a", false)
	if err := join(m, s.Id, "b"); err != nil {
		t.Fatalf("join: %v", err)
	}
	// Plain unfriend is not a block: the stay continues until rejoin.
	delete(fc.friends, [2]string{"a", "b"})
	delete(fc.friends, [2]string{"b", "a"})
	if _, err := m.Heartbeat(context.Background(), "xbox", s.Id, "b", 600); err != nil {
		t.Fatalf("unfriended participant evicted mid-call: %v", err)
	}
}
