// Package session is the voice service's rules: who owns a session, who may
// join, how long a participant stays admitted, and what each change announces.
// It holds state in memory on purpose: a session is as live as its participants'
// leases, a restart drops them, and adapters re-create (the same trade as the
// website's voice hub it will replace). The account core is the only authority
// on friendship, blocks and account status.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/openpak/voice/internal/core"
	voicev1 "github.com/openpak/voice/proto/openpak/voice/v1"
)

const (
	MaxParticipants = 16 // the original Xbox ceiling; WebRTC meshes stay lower until an SFU exists
	DefaultLease    = 120 * time.Second
	MaxLease        = 4 * time.Hour
	DefaultInvite   = 10 * time.Minute
	endedKeep       = 5 * time.Minute
	maxSessions     = 5000
	maxPerAccount   = 8
	maxInvites      = 64
)

var (
	ErrNotFound       = errors.New("voice: no such session")
	ErrEnded          = errors.New("voice: session ended")
	ErrFull           = errors.New("voice: session is full")
	ErrNotOwner       = errors.New("voice: only the owner may do that")
	ErrNotParticipant = errors.New("voice: not a participant")
	ErrForeign        = errors.New("voice: participant belongs to another adapter")
	ErrInvalid        = errors.New("voice: invalid request")
	ErrConflict       = errors.New("voice: native reference already in use")
	ErrQuota          = errors.New("voice: too many sessions")
	ErrUnavailable    = errors.New("voice: account core unavailable")
)

// DeniedError is the account core's refusal, carried to the adapter so it can
// map the reason to its native error.
type DeniedError struct{ Reason string }

func (e DeniedError) Error() string { return "voice: denied: " + e.Reason }

// Authorizer answers directed interaction questions (the account core).
type Authorizer interface {
	Check(ctx context.Context, actor, target string, k core.Kind) (core.Decision, error)
}

// Announcer places an event on the account core's stream. Best effort.
type Announcer interface {
	Announce(ctx context.Context, eventType, toAccount, sessionID string) error
}

type part struct {
	account   string
	adapter   string
	transport voicev1.Transport
	codecs    []*voicev1.Codec
	muted     bool
	joined    time.Time
	expires   time.Time
}

type sess struct {
	id         string
	kind       voicev1.SessionKind
	owner      string
	max        int
	endOnOwner bool
	title      string
	refs       map[string]string
	parts      map[string]*part
	invites    map[string]time.Time
	created    time.Time
	ended      time.Time
	creator    string // adapter namespace that created it
	watchers   map[chan *voicev1.SessionEvent]struct{}
}

// Manager holds every live session.
type Manager struct {
	Auth     Authorizer
	Announce Announcer
	Now      func() time.Time

	mu       sync.Mutex
	sessions map[string]*sess
	byRef    map[string]string // ns \x00 ref -> id
	idem     map[string]string // adapter \x00 key -> id
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) init() {
	if m.sessions == nil {
		m.sessions = map[string]*sess{}
		m.byRef = map[string]string{}
		m.idem = map[string]string{}
	}
}

func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func clampLease(sec uint32) time.Duration {
	d := time.Duration(sec) * time.Second
	if d <= 0 {
		return DefaultLease
	}
	return min(d, MaxLease)
}

// proto renders the session. Callers hold m.mu.
func (s *sess) proto() *voicev1.Session {
	out := &voicev1.Session{
		Id: s.id, Kind: s.kind, OwnerAccount: s.owner, MaxParticipants: uint32(s.max),
		EndWhenOwnerLeaves: s.endOnOwner, Title: s.title, CreatedUnix: s.created.Unix(),
		NativeRefs: map[string]string{},
	}
	for k, v := range s.refs {
		out.NativeRefs[k] = v
	}
	if !s.ended.IsZero() {
		out.EndedUnix = s.ended.Unix()
	}
	ps := make([]*part, 0, len(s.parts))
	for _, p := range s.parts {
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool {
		if !ps[i].joined.Equal(ps[j].joined) {
			return ps[i].joined.Before(ps[j].joined)
		}
		return ps[i].account < ps[j].account
	})
	for _, p := range ps {
		out.Participants = append(out.Participants, &voicev1.Participant{
			AccountId: p.account, Adapter: p.adapter, Transport: p.transport, Codecs: p.codecs,
			Muted: p.muted, JoinedUnix: p.joined.Unix(), LeaseExpiresUnix: p.expires.Unix(),
		})
	}
	return out
}

// emit tells the watchers. Callers hold m.mu. A watcher that cannot keep up is
// dropped (its stream ends and the client resubscribes for a fresh snapshot).
func (s *sess) emit(t voicev1.SessionEvent_Type, account, reason string) {
	ev := &voicev1.SessionEvent{Type: t, AccountId: account, Reason: reason, Session: s.proto()}
	for ch := range s.watchers {
		select {
		case ch <- ev:
		default:
			delete(s.watchers, ch)
			close(ch)
		}
	}
}

type CreateParams struct {
	Adapter        string
	Owner          string
	Kind           voicev1.SessionKind
	Max            uint32
	EndOnOwnerLeft bool
	Title          string
	Transport      voicev1.Transport
	Codecs         []*voicev1.Codec
	IdempotencyKey string
	NativeRef      string
}

// Create makes a session and joins its owner. The adapter vouches that the owner
// is the account it authenticated.
func (m *Manager) Create(p CreateParams) (*voicev1.Session, bool, error) {
	if p.Owner == "" || p.Adapter == "" {
		return nil, false, ErrInvalid
	}
	max := int(p.Max)
	if max == 0 || max > MaxParticipants {
		max = MaxParticipants
	}
	if max < 2 {
		return nil, false, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	if p.IdempotencyKey != "" {
		if id, ok := m.idem[p.Adapter+"\x00"+p.IdempotencyKey]; ok {
			if s := m.sessions[id]; s != nil && s.ended.IsZero() {
				return s.proto(), true, nil
			}
		}
	}
	live, owned := 0, 0
	for _, s := range m.sessions {
		if s.ended.IsZero() {
			live++
			if _, in := s.parts[p.Owner]; in {
				owned++
			}
		}
	}
	if live >= maxSessions || owned >= maxPerAccount {
		return nil, false, ErrQuota
	}
	now := m.now()
	s := &sess{
		id: newID(), kind: p.Kind, owner: p.Owner, max: max, endOnOwner: p.EndOnOwnerLeft, title: p.Title,
		refs: map[string]string{}, parts: map[string]*part{}, invites: map[string]time.Time{},
		created: now, creator: p.Adapter, watchers: map[chan *voicev1.SessionEvent]struct{}{},
	}
	if p.NativeRef != "" {
		key := p.Adapter + "\x00" + p.NativeRef
		if _, taken := m.byRef[key]; taken {
			return nil, false, ErrConflict
		}
		s.refs[p.Adapter] = p.NativeRef
		m.byRef[key] = s.id
	}
	s.parts[p.Owner] = &part{account: p.Owner, adapter: p.Adapter, transport: p.Transport, codecs: p.Codecs,
		joined: now, expires: now.Add(DefaultLease)}
	m.sessions[s.id] = s
	if p.IdempotencyKey != "" {
		m.idem[p.Adapter+"\x00"+p.IdempotencyKey] = s.id
	}
	return s.proto(), false, nil
}

func (m *Manager) get(id string) (*sess, error) {
	m.init()
	s := m.sessions[id]
	if s == nil {
		return nil, ErrNotFound
	}
	return s, nil
}

// Get returns a session, live or recently ended.
func (m *Manager) Get(id string) (*voicev1.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.get(id)
	if err != nil {
		return nil, err
	}
	return s.proto(), nil
}

// ByNativeRef resolves an adapter's native identifier.
func (m *Manager) ByNativeRef(ns, ref string) (*voicev1.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	id, ok := m.byRef[ns+"\x00"+ref]
	if !ok {
		return nil, ErrNotFound
	}
	return m.sessions[id].proto(), nil
}

// List returns the live sessions an account participates in.
func (m *Manager) List(account string) []*voicev1.Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	var out []*voicev1.Session
	for _, s := range m.sessions {
		if _, in := s.parts[account]; in && s.ended.IsZero() {
			out = append(out, s.proto())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return out
}

// SetNativeRef binds an adapter's identifier to a session.
func (m *Manager) SetNativeRef(id, ns, ref string) error {
	if ns == "" || ref == "" {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.get(id)
	if err != nil {
		return err
	}
	if !s.ended.IsZero() {
		return ErrEnded
	}
	key := ns + "\x00" + ref
	if other, taken := m.byRef[key]; taken && other != id {
		return ErrConflict
	}
	if old, had := s.refs[ns]; had && old != ref {
		delete(m.byRef, ns+"\x00"+old)
	}
	s.refs[ns] = ref
	m.byRef[key] = id
	return nil
}

// blocks reports whether a refusal reason is a hard one that an invitation does
// not override: a block or an inactive account.
func hard(reason string) bool { return reason == core.ReasonBlocked || reason == core.ReasonInactive }

type JoinParams struct {
	Adapter   string
	SessionID string
	Account   string
	Transport voicev1.Transport
	Codecs    []*voicev1.Codec
	LeaseSec  uint32
}

// Join admits an account (or renews it).
func (m *Manager) Join(ctx context.Context, p JoinParams) (*voicev1.Session, bool, error) {
	if p.Account == "" || p.Adapter == "" {
		return nil, false, ErrInvalid
	}
	m.mu.Lock()
	s, err := m.get(p.SessionID)
	if err != nil {
		m.mu.Unlock()
		return nil, false, err
	}
	if !s.ended.IsZero() {
		m.mu.Unlock()
		return nil, false, ErrEnded
	}
	if cur, in := s.parts[p.Account]; in {
		if cur.adapter != p.Adapter {
			m.mu.Unlock()
			return nil, false, ErrForeign
		}
		cur.expires = m.now().Add(clampLease(p.LeaseSec))
		if p.Transport != voicev1.Transport_TRANSPORT_UNSPECIFIED {
			cur.transport = p.Transport
		}
		if p.Codecs != nil {
			cur.codecs = p.Codecs
		}
		out := s.proto()
		m.mu.Unlock()
		return out, true, nil
	}
	owner := s.owner
	invited := false
	if exp, ok := s.invites[p.Account]; ok && exp.After(m.now()) {
		invited = true
	}
	present := make([]string, 0, len(s.parts))
	for a := range s.parts {
		present = append(present, a)
	}
	sort.Strings(present)
	m.mu.Unlock()

	// Decisions come from the account core without the lock held. Any failure
	// to get an answer refuses the join: voice never guesses a friendship.
	if p.Account != owner {
		d, err := m.Auth.Check(ctx, p.Account, owner, core.Discover)
		if err != nil {
			return nil, false, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		if !d.Allowed && (!invited || hard(d.Reason)) {
			return nil, false, DeniedError{Reason: d.Reason}
		}
	}
	for _, other := range present {
		if other == p.Account {
			continue
		}
		for _, dir := range [2][2]string{{p.Account, other}, {other, p.Account}} {
			d, err := m.Auth.Check(ctx, dir[0], dir[1], core.Discover)
			if err != nil {
				return nil, false, fmt.Errorf("%w: %v", ErrUnavailable, err)
			}
			if !d.Allowed && hard(d.Reason) {
				return nil, false, DeniedError{Reason: d.Reason}
			}
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	s, err = m.get(p.SessionID)
	if err != nil {
		return nil, false, err
	}
	if !s.ended.IsZero() {
		return nil, false, ErrEnded
	}
	if _, in := s.parts[p.Account]; in { // raced with a concurrent join of the same account
		s.parts[p.Account].expires = m.now().Add(clampLease(p.LeaseSec))
		return s.proto(), true, nil
	}
	if len(s.parts) >= s.max {
		return nil, false, ErrFull
	}
	n := 0
	for _, o := range m.sessions {
		if _, in := o.parts[p.Account]; in && o.ended.IsZero() {
			n++
		}
	}
	if n >= maxPerAccount {
		return nil, false, ErrQuota
	}
	now := m.now()
	s.parts[p.Account] = &part{account: p.Account, adapter: p.Adapter, transport: p.Transport, codecs: p.Codecs,
		joined: now, expires: now.Add(clampLease(p.LeaseSec))}
	delete(s.invites, p.Account)
	s.emit(voicev1.SessionEvent_TYPE_JOINED, p.Account, "")
	return s.proto(), false, nil
}

// Heartbeat renews a participant's lease, re-checking the relationships that
// admitted it: a block, ban or deactivation since joining ends the stay here,
// not at some later rejoin. Only hard denials (blocked, inactive) evict — a
// plain unfriend takes effect at rejoin, not mid-call. An unanswerable core
// keeps the lease: renewals must survive a core blip (a mass drop is worse
// than a delayed eviction, and the next heartbeat asks again).
func (m *Manager) Heartbeat(ctx context.Context, adapter, id, account string, leaseSec uint32) (time.Time, error) {
	m.mu.Lock()
	s, err := m.get(id)
	if err != nil {
		m.mu.Unlock()
		return time.Time{}, err
	}
	if !s.ended.IsZero() {
		m.mu.Unlock()
		return time.Time{}, ErrEnded
	}
	p := s.parts[account]
	if p == nil {
		m.mu.Unlock()
		return time.Time{}, ErrNotParticipant
	}
	if p.adapter != adapter {
		m.mu.Unlock()
		return time.Time{}, ErrForeign
	}
	owner := s.owner
	present := make([]string, 0, len(s.parts))
	for a := range s.parts {
		present = append(present, a)
	}
	m.mu.Unlock()

	if err := m.recheck(ctx, owner, account, present); err != nil {
		m.mu.Lock()
		m.remove(s, account, "authz_revoked")
		m.mu.Unlock()
		return time.Time{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	s, err = m.get(id)
	if err != nil {
		return time.Time{}, err
	}
	if !s.ended.IsZero() {
		return time.Time{}, ErrEnded
	}
	p = s.parts[account]
	if p == nil {
		return time.Time{}, ErrNotParticipant
	}
	p.expires = m.now().Add(clampLease(leaseSec))
	return p.expires, nil
}

// recheck applies the join-time relationship rules to a participant that is
// already in: any hard denial ends the stay. Core errors are not denials.
func (m *Manager) recheck(ctx context.Context, owner, account string, present []string) error {
	others := map[string]bool{owner: true}
	for _, a := range present {
		others[a] = true
	}
	delete(others, account)
	for other := range others {
		for _, dir := range [2][2]string{{account, other}, {other, account}} {
			d, err := m.Auth.Check(ctx, dir[0], dir[1], core.Discover)
			if err != nil {
				return nil
			}
			if !d.Allowed && hard(d.Reason) {
				return DeniedError{Reason: d.Reason}
			}
		}
	}
	return nil
}

// remove drops a participant and applies the owner rules. Callers hold m.mu.
func (m *Manager) remove(s *sess, account, reason string) {
	if _, in := s.parts[account]; !in {
		return
	}
	delete(s.parts, account)
	s.emit(voicev1.SessionEvent_TYPE_LEFT, account, reason)
	if account == s.owner {
		if s.endOnOwner {
			m.end(s, "owner_left")
			return
		}
		var next *part
		for _, p := range s.parts {
			if next == nil || p.joined.Before(next.joined) || (p.joined.Equal(next.joined) && p.account < next.account) {
				next = p
			}
		}
		if next != nil {
			s.owner = next.account
		}
	}
	if len(s.parts) == 0 {
		m.end(s, "empty")
	}
}

func (m *Manager) end(s *sess, reason string) {
	if !s.ended.IsZero() {
		return
	}
	s.ended = m.now()
	for a := range s.parts {
		delete(s.parts, a)
	}
	s.emit(voicev1.SessionEvent_TYPE_ENDED, "", reason)
	for ch := range s.watchers {
		close(ch)
		delete(s.watchers, ch)
	}
	for ns, ref := range s.refs {
		delete(m.byRef, ns+"\x00"+ref)
	}
}

// Leave removes an account that the calling adapter admitted.
func (m *Manager) Leave(adapter, id, account string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.get(id)
	if err != nil {
		return err
	}
	if !s.ended.IsZero() {
		return ErrEnded
	}
	p := s.parts[account]
	if p == nil {
		return ErrNotParticipant
	}
	if p.adapter != adapter {
		return ErrForeign
	}
	m.remove(s, account, "left")
	return nil
}

// End ends the session for everybody. Owner only.
func (m *Manager) End(id, actor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.get(id)
	if err != nil {
		return err
	}
	if !s.ended.IsZero() {
		return ErrEnded
	}
	if s.owner != actor {
		return ErrNotOwner
	}
	m.end(s, "ended")
	return nil
}

// Kick removes a participant. Owner only; the owner cannot kick itself.
func (m *Manager) Kick(id, actor, account string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.get(id)
	if err != nil {
		return err
	}
	if !s.ended.IsZero() {
		return ErrEnded
	}
	if s.owner != actor {
		return ErrNotOwner
	}
	if account == actor {
		return ErrInvalid
	}
	if s.parts[account] == nil {
		return ErrNotParticipant
	}
	m.remove(s, account, "kicked")
	return nil
}

// SetMuted changes a participant's mute flag (the adapter that admitted it).
func (m *Manager) SetMuted(adapter, id, account string, muted bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.get(id)
	if err != nil {
		return err
	}
	if !s.ended.IsZero() {
		return ErrEnded
	}
	p := s.parts[account]
	if p == nil {
		return ErrNotParticipant
	}
	if p.adapter != adapter {
		return ErrForeign
	}
	if p.muted != muted {
		p.muted = muted
		s.emit(voicev1.SessionEvent_TYPE_MUTED, account, "")
	}
	return nil
}

// Invite records an invitation and announces it on the account core's stream.
func (m *Manager) Invite(ctx context.Context, id, actor, target string, ttl time.Duration) (duplicate bool, err error) {
	if target == "" || actor == target {
		return false, ErrInvalid
	}
	if ttl <= 0 {
		ttl = DefaultInvite
	}
	m.mu.Lock()
	s, err := m.get(id)
	if err != nil {
		m.mu.Unlock()
		return false, err
	}
	if !s.ended.IsZero() {
		m.mu.Unlock()
		return false, ErrEnded
	}
	if s.parts[actor] == nil {
		m.mu.Unlock()
		return false, ErrNotParticipant
	}
	if s.parts[target] != nil {
		m.mu.Unlock()
		return false, ErrInvalid
	}
	m.mu.Unlock()

	d, err := m.Auth.Check(ctx, actor, target, core.Invite)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if !d.Allowed {
		return false, DeniedError{Reason: d.Reason}
	}

	m.mu.Lock()
	s, err = m.get(id)
	if err != nil {
		m.mu.Unlock()
		return false, err
	}
	if !s.ended.IsZero() {
		m.mu.Unlock()
		return false, ErrEnded
	}
	now := m.now()
	for a, exp := range s.invites {
		if !exp.After(now) {
			delete(s.invites, a)
		}
	}
	if exp, ok := s.invites[target]; ok && exp.After(now) {
		duplicate = true
	} else if len(s.invites) >= maxInvites {
		m.mu.Unlock()
		return false, ErrQuota
	}
	s.invites[target] = now.Add(ttl)
	s.emit(voicev1.SessionEvent_TYPE_INVITED, target, "")
	m.mu.Unlock()

	if !duplicate && m.Announce != nil {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		// Storage is not best effort, announcement is: the invitation is held
		// here whether or not the stream took it.
		_ = m.Announce.Announce(actx, "voice_invite", target, id)
	}
	return duplicate, nil
}

// Watch subscribes to a session. The first event is a snapshot; the channel
// closes when the session ends, the watcher is dropped for lagging, or cancel
// is called.
func (m *Manager) Watch(id string) (<-chan *voicev1.SessionEvent, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.get(id)
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan *voicev1.SessionEvent, 64)
	ch <- &voicev1.SessionEvent{Type: voicev1.SessionEvent_TYPE_SNAPSHOT, Session: s.proto()}
	if !s.ended.IsZero() {
		close(ch)
		return ch, func() {}, nil
	}
	s.watchers[ch] = struct{}{}
	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, ok := s.watchers[ch]; ok {
			delete(s.watchers, ch)
			close(ch)
		}
	}, nil
}

// Reap removes expired participants and invitations and forgets sessions that
// ended a while ago. Run it on a ticker.
func (m *Manager) Reap() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	now := m.now()
	for id, s := range m.sessions {
		if !s.ended.IsZero() {
			if now.Sub(s.ended) > endedKeep {
				delete(m.sessions, id)
				for k, v := range m.idem {
					if v == id {
						delete(m.idem, k)
					}
				}
			}
			continue
		}
		accounts := make([]string, 0, len(s.parts))
		for a := range s.parts {
			accounts = append(accounts, a)
		}
		sort.Strings(accounts)
		for _, a := range accounts {
			if p := s.parts[a]; p != nil && !p.expires.After(now) {
				m.remove(s, a, "lease_expired")
			}
		}
		for a, exp := range s.invites {
			if !exp.After(now) {
				delete(s.invites, a)
			}
		}
	}
}

// Run reaps until ctx ends.
func (m *Manager) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Reap()
		}
	}
}

// Counts reports live sessions and participants for the metrics endpoint.
func (m *Manager) Counts() (sessions, participants int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.ended.IsZero() {
			sessions++
			participants += len(s.parts)
		}
	}
	return
}
