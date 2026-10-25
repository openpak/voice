package core_test

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	accountv1 "github.com/openpak/account-api/openpak/account/v1"
	"github.com/openpak/voice/internal/core"
)

type social struct {
	accountv1.UnimplementedSocialServer
	key    string
	reason accountv1.DenialReason
	allow  bool
	kinds  []accountv1.InteractionKind
}

func (s *social) CheckInteraction(ctx context.Context, r *accountv1.CheckInteractionRequest) (*accountv1.CheckInteractionResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("x-internal-key"); len(v) == 0 || v[0] != s.key {
		panic("missing credential")
	}
	s.kinds = append(s.kinds, r.GetKind())
	return &accountv1.CheckInteractionResponse{Allowed: s.allow, Reason: s.reason}, nil
}

type events struct {
	accountv1.UnimplementedEventsServer
	got []*accountv1.EmitEventRequest
}

func (e *events) EmitEvent(_ context.Context, r *accountv1.EmitEventRequest) (*accountv1.EmitEventResponse, error) {
	e.got = append(e.got, r)
	return &accountv1.EmitEventResponse{}, nil
}

func TestCheckAndAnnounce(t *testing.T) {
	soc, ev := &social{key: "k"}, &events{}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	accountv1.RegisterSocialServer(gs, soc)
	accountv1.RegisterEventsServer(gs, ev)
	go gs.Serve(lis)
	defer gs.Stop()
	cc, err := grpc.NewClient("passthrough:///b", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	c := core.New(accountv1.NewSocialClient(cc), accountv1.NewEventsClient(cc), "k")

	for _, tc := range []struct {
		reason accountv1.DenialReason
		want   string
	}{
		{accountv1.DenialReason_DENIAL_REASON_BLOCKED, core.ReasonBlocked},
		{accountv1.DenialReason_DENIAL_REASON_NOT_FRIENDS, core.ReasonNotFriends},
		{accountv1.DenialReason_DENIAL_REASON_ACCOUNT_INACTIVE, core.ReasonInactive},
		{accountv1.DenialReason_DENIAL_REASON_SELF, core.ReasonSelf},
	} {
		soc.reason = tc.reason
		d, err := c.Check(context.Background(), "a", "b", core.Discover)
		if err != nil || d.Allowed || d.Reason != tc.want {
			t.Fatalf("%v: %+v %v", tc.reason, d, err)
		}
	}
	soc.allow = true
	if d, err := c.Check(context.Background(), "a", "b", core.Invite); err != nil || !d.Allowed {
		t.Fatalf("allowed: %+v %v", d, err)
	}
	if k := soc.kinds; k[0] != accountv1.InteractionKind_INTERACTION_KIND_DISCOVER || k[len(k)-1] != accountv1.InteractionKind_INTERACTION_KIND_INVITE {
		t.Fatalf("kinds: %v", k)
	}

	if err := c.Announce(context.Background(), "voice_invite", "b", "sid"); err != nil {
		t.Fatal(err)
	}
	if len(ev.got) != 1 || ev.got[0].Type != "voice_invite" || ev.got[0].AccountId != "b" || ev.got[0].SubjectId != "sid" {
		t.Fatalf("event: %v", ev.got)
	}
}
