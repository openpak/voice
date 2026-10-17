// Package core is the voice service's client of the account core. The core is
// consulted for exactly two things: whether one account may interact with
// another (friendship, blocks, account status: its one answer, the same for
// every surface) and where invitations are announced (the universal event
// stream every adapter consumes). Voice never stores a friendship or a block.
package core

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	accountv1 "github.com/openpak/voice/internal/accountpb"
)

// Kind is the interaction being asked about.
type Kind int

const (
	Discover Kind = iota // joining someone's session
	Invite               // inviting someone
)

// Denial reasons, as the account core names them.
const (
	ReasonBlocked    = "BLOCKED"
	ReasonNotFriends = "NOT_FRIENDS"
	ReasonInactive   = "ACCOUNT_INACTIVE"
	ReasonSelf       = "SELF"
)

// Decision is the core's answer for a directed interaction.
type Decision struct {
	Allowed bool
	Reason  string // empty when allowed
}

// Client talks to the account core over gRPC.
type Client struct {
	social accountv1.SocialClient
	events accountv1.EventsClient
	key    string
}

// Dial opens the core connection. Plaintext on purpose: the internal network is
// the boundary, as for every other internal caller.
func Dial(addr, internalKey string) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("voice: core dial: %w", err)
	}
	return New(accountv1.NewSocialClient(conn), accountv1.NewEventsClient(conn), internalKey), nil
}

// New builds a client on existing stubs; tests hand it bufconn-backed ones.
func New(social accountv1.SocialClient, events accountv1.EventsClient, internalKey string) *Client {
	return &Client{social: social, events: events, key: internalKey}
}

func (c *Client) ctx(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	return metadata.AppendToOutgoingContext(ctx, "x-internal-key", c.key), cancel
}

// Check asks whether actor may interact with target.
func (c *Client) Check(ctx context.Context, actor, target string, k Kind) (Decision, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	kind := accountv1.InteractionKind_INTERACTION_KIND_DISCOVER
	if k == Invite {
		kind = accountv1.InteractionKind_INTERACTION_KIND_INVITE
	}
	r, err := c.social.CheckInteraction(ctx, &accountv1.CheckInteractionRequest{ActorId: actor, TargetId: target, Kind: kind})
	if err != nil {
		return Decision{}, err
	}
	if r.GetAllowed() {
		return Decision{Allowed: true}, nil
	}
	switch r.GetReason() {
	case accountv1.DenialReason_DENIAL_REASON_BLOCKED:
		return Decision{Reason: ReasonBlocked}, nil
	case accountv1.DenialReason_DENIAL_REASON_NOT_FRIENDS:
		return Decision{Reason: ReasonNotFriends}, nil
	case accountv1.DenialReason_DENIAL_REASON_ACCOUNT_INACTIVE:
		return Decision{Reason: ReasonInactive}, nil
	case accountv1.DenialReason_DENIAL_REASON_SELF:
		return Decision{Reason: ReasonSelf}, nil
	}
	return Decision{Reason: "DENIED"}, nil
}

// Announce places an event on the core's universal stream: account_id = who must
// hear it, subject_id = the voice session id. No audio or session body rides the
// event log; receivers fetch the session here.
func (c *Client) Announce(ctx context.Context, eventType, toAccount, sessionID string) error {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	_, err := c.events.EmitEvent(ctx, &accountv1.EmitEventRequest{Type: eventType, AccountId: toAccount, SubjectId: sessionID})
	return err
}
