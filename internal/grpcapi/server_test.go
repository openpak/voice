package grpcapi_test

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/openpak/voice/internal/core"
	"github.com/openpak/voice/internal/grpcapi"
	"github.com/openpak/voice/internal/session"
	voicev1 "github.com/openpak/voice/proto/openpak/voice/v1"
)

type auth struct{ friends map[string]bool }

func (a auth) Check(_ context.Context, actor, target string, _ core.Kind) (core.Decision, error) {
	if a.friends[actor+">"+target] {
		return core.Decision{Allowed: true}, nil
	}
	return core.Decision{Reason: core.ReasonNotFriends}, nil
}

func start(t *testing.T, friends ...string) voicev1.VoiceClient {
	t.Helper()
	fr := map[string]bool{}
	for _, f := range friends {
		fr[f] = true
	}
	api := &grpcapi.Server{M: &session.Manager{Auth: auth{fr}}, Keys: grpcapi.Keys{"xbox": "xbox-key-0123456789", "app": "app-key-0123456789"}}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.UnaryInterceptor(api.Unary()), grpc.StreamInterceptor(api.Stream()))
	voicev1.RegisterVoiceServer(gs, api)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	cc, err := grpc.NewClient("passthrough:///bufnet", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	return voicev1.NewVoiceClient(cc)
}

func as(key string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "x-internal-key", key)
}

func code(err error) codes.Code { return status.Code(err) }

func TestCredentialsAndNamespaces(t *testing.T) {
	c := start(t, "b>a", "c>a")
	if _, err := c.GetSession(context.Background(), &voicev1.GetSessionRequest{SessionId: "x"}); code(err) != codes.Unauthenticated {
		t.Fatalf("no key: %v", err)
	}
	if _, err := c.GetSession(as("wrong-key-0123456789"), &voicev1.GetSessionRequest{SessionId: "x"}); code(err) != codes.Unauthenticated {
		t.Fatalf("bad key: %v", err)
	}

	cr, err := c.CreateSession(as("xbox-key-0123456789"), &voicev1.CreateSessionRequest{OwnerAccount: "a", NativeRef: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	id := cr.Session.Id
	if cr.Session.Participants[0].Adapter != "xbox" {
		t.Fatalf("the namespace comes from the key: %v", cr.Session.Participants)
	}

	// The app adapter joins its own participant; xbox cannot touch it.
	if _, err := c.JoinSession(as("app-key-0123456789"), &voicev1.JoinSessionRequest{SessionId: id, AccountId: "b", Transport: voicev1.Transport_TRANSPORT_WEBRTC}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LeaveSession(as("xbox-key-0123456789"), &voicev1.LeaveSessionRequest{SessionId: id, AccountId: "b"}); code(err) != codes.PermissionDenied {
		t.Fatalf("xbox leaving the app's participant: %v", err)
	}
	if _, err := c.SetMuted(as("xbox-key-0123456789"), &voicev1.SetMutedRequest{SessionId: id, AccountId: "b", Muted: true}); code(err) != codes.PermissionDenied {
		t.Fatalf("xbox muting the app's participant: %v", err)
	}
	if _, err := c.SetNativeRef(as("app-key-0123456789"), &voicev1.SetNativeRefRequest{SessionId: id, Namespace: "xbox", NativeRef: "zzz"}); code(err) != codes.PermissionDenied {
		t.Fatalf("setting another adapter's reference: %v", err)
	}
	if _, err := c.SetNativeRef(as("app-key-0123456789"), &voicev1.SetNativeRefRequest{SessionId: id, Namespace: "app", NativeRef: "room-1"}); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetSessionByNativeRef(as("xbox-key-0123456789"), &voicev1.GetSessionByNativeRefRequest{Namespace: "app", NativeRef: "room-1"})
	if err != nil || got.Session.Id != id || len(got.Session.Participants) != 2 {
		t.Fatalf("an adapter can read another's reference: %v %v", got, err)
	}

	// Denials carry the core's reason.
	_, err = c.JoinSession(as("xbox-key-0123456789"), &voicev1.JoinSessionRequest{SessionId: id, AccountId: "stranger"})
	if code(err) != codes.PermissionDenied || status.Convert(err).Message() != core.ReasonNotFriends {
		t.Fatalf("stranger: %v", err)
	}
	if _, err := c.GetSession(as("xbox-key-0123456789"), &voicev1.GetSessionRequest{SessionId: "missing"}); code(err) != codes.NotFound {
		t.Fatalf("missing: %v", err)
	}
}

func TestWatchFollowsASession(t *testing.T) {
	c := start(t, "b>a")
	key := "xbox-key-0123456789"
	cr, err := c.CreateSession(as(key), &voicev1.CreateSessionRequest{OwnerAccount: "a"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(as(key), 5*time.Second)
	defer cancel()
	st, err := c.WatchSession(ctx, &voicev1.WatchSessionRequest{SessionId: cr.Session.Id})
	if err != nil {
		t.Fatal(err)
	}
	if ev, err := st.Recv(); err != nil || ev.Type != voicev1.SessionEvent_TYPE_SNAPSHOT {
		t.Fatalf("snapshot: %v %v", ev, err)
	}
	if _, err := c.JoinSession(as(key), &voicev1.JoinSessionRequest{SessionId: cr.Session.Id, AccountId: "b"}); err != nil {
		t.Fatal(err)
	}
	if ev, err := st.Recv(); err != nil || ev.Type != voicev1.SessionEvent_TYPE_JOINED || ev.AccountId != "b" {
		t.Fatalf("joined: %v %v", ev, err)
	}
	if _, err := c.EndSession(as(key), &voicev1.EndSessionRequest{SessionId: cr.Session.Id, ActorAccount: "a"}); err != nil {
		t.Fatal(err)
	}
	if ev, err := st.Recv(); err != nil || ev.Type != voicev1.SessionEvent_TYPE_ENDED {
		t.Fatalf("ended: %v %v", ev, err)
	}
}

func TestParseKeys(t *testing.T) {
	if _, err := grpcapi.ParseKeys(""); err == nil {
		t.Fatal("no keys is an error")
	}
	if _, err := grpcapi.ParseKeys("xbox=short"); err == nil {
		t.Fatal("short keys are refused")
	}
	if _, err := grpcapi.ParseKeys("xbox=0123456789abcdef,xbox=fedcba9876543210"); err == nil {
		t.Fatal("duplicate namespaces are refused")
	}
	k, err := grpcapi.ParseKeys("xbox=0123456789abcdef, app=fedcba9876543210")
	if err != nil || len(k) != 2 {
		t.Fatal(k, err)
	}
}
