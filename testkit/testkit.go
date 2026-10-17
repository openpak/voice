// Package testkit runs the real voice service in process for adapters' tests
// (xb-voice's end-to-end tests), outside this module's internal/ tree. Only the
// account core is replaced, by a small friend/block table. Test support only;
// no command imports it.
package testkit

import (
	"context"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/openpak/voice/internal/core"
	"github.com/openpak/voice/internal/grpcapi"
	"github.com/openpak/voice/internal/session"
	voicev1 "github.com/openpak/voice/proto/openpak/voice/v1"
)

// Core is the stand-in account core: friendships, blocks and inactive accounts.
type Core struct {
	mu       sync.Mutex
	friends  map[[2]string]bool
	blocks   map[[2]string]bool
	inactive map[string]bool
	events   []string
}

// Befriend makes a and b friends.
func (c *Core) Befriend(a, b string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.friends[[2]string{a, b}], c.friends[[2]string{b, a}] = true, true
}

// Block records that blocker blocked blocked.
func (c *Core) Block(blocker, blocked string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocks[[2]string{blocker, blocked}] = true
}

// Deactivate marks an account banned or deleted.
func (c *Core) Deactivate(a string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inactive[a] = true
}

// Events returns the announcements made so far as "type:account:subject".
func (c *Core) Events() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

func (c *Core) Check(_ context.Context, actor, target string, _ core.Kind) (core.Decision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case actor == target:
		return core.Decision{Reason: core.ReasonSelf}, nil
	case c.inactive[actor] || c.inactive[target]:
		return core.Decision{Reason: core.ReasonInactive}, nil
	case c.blocks[[2]string{actor, target}] || c.blocks[[2]string{target, actor}]:
		return core.Decision{Reason: core.ReasonBlocked}, nil
	case c.friends[[2]string{actor, target}]:
		return core.Decision{Allowed: true}, nil
	}
	return core.Decision{Reason: core.ReasonNotFriends}, nil
}

func (c *Core) Announce(_ context.Context, typ, to, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, typ+":"+to+":"+id)
	return nil
}

// Adapter keys the service accepts in New.
const (
	XboxKey = "testkit-xbox-key-0123456789"
	AppKey  = "testkit-app-key-0123456789"
)

// Service is one running voice service with its stand-in account core.
type Service struct {
	Core *Core
	lis  *bufconn.Listener
	t    testing.TB
}

// NewService starts a voice service with an "xbox" and an "app" adapter.
func NewService(t testing.TB) *Service {
	t.Helper()
	c := &Core{friends: map[[2]string]bool{}, blocks: map[[2]string]bool{}, inactive: map[string]bool{}}
	api := &grpcapi.Server{M: &session.Manager{Auth: c, Announce: c}, Keys: grpcapi.Keys{"xbox": XboxKey, "app": AppKey}}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.UnaryInterceptor(api.Unary()), grpc.StreamInterceptor(api.Stream()))
	voicev1.RegisterVoiceServer(gs, api)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	return &Service{Core: c, lis: lis, t: t}
}

// Client returns a client that presents the given adapter key (XboxKey or AppKey).
func (s *Service) Client(adapterKey string) voicev1.VoiceClient {
	s.t.Helper()
	cc, err := grpc.NewClient("passthrough:///voice", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return s.lis.Dial() }),
		grpc.WithUnaryInterceptor(func(ctx context.Context, m string, req, rep any, cc *grpc.ClientConn, inv grpc.UnaryInvoker, o ...grpc.CallOption) error {
			return inv(metadata.AppendToOutgoingContext(ctx, "x-internal-key", adapterKey), m, req, rep, cc, o...)
		}),
		grpc.WithStreamInterceptor(func(ctx context.Context, d *grpc.StreamDesc, cc *grpc.ClientConn, m string, st grpc.Streamer, o ...grpc.CallOption) (grpc.ClientStream, error) {
			return st(metadata.AppendToOutgoingContext(ctx, "x-internal-key", adapterKey), d, cc, m, o...)
		}))
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { cc.Close() })
	return voicev1.NewVoiceClient(cc)
}

// New starts a service and returns a client for one adapter plus the stand-in core.
func New(t testing.TB, adapterKey string) (voicev1.VoiceClient, *Core) {
	t.Helper()
	s := NewService(t)
	return s.Client(adapterKey), s.Core
}
