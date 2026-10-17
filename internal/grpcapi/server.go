// Package grpcapi is the voice service's gRPC surface for platform adapters.
// Every call presents an adapter key in x-internal-key; the key names the
// adapter's namespace, and that namespace is stamped on everything the adapter
// admits, so one adapter can never alter another's participants.
package grpcapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/openpak/voice/internal/session"
	voicev1 "github.com/openpak/voice/proto/openpak/voice/v1"
)

// Keys maps adapter namespaces to their keys.
type Keys map[string]string

// ParseKeys reads "xbox=key,app=key". Empty keys and bad namespaces are errors,
// so a typo cannot open an unauthenticated adapter.
func ParseKeys(s string) (Keys, error) {
	out := Keys{}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		ns, key, ok := strings.Cut(kv, "=")
		ns, key = strings.TrimSpace(ns), strings.TrimSpace(key)
		if !ok || ns == "" || len(key) < 16 || strings.ContainsAny(ns, " \x00") {
			return nil, errors.New("voice: adapter keys are namespace=key (key of at least 16 characters)")
		}
		if _, dup := out[ns]; dup {
			return nil, errors.New("voice: duplicate adapter namespace " + ns)
		}
		out[ns] = key
	}
	if len(out) == 0 {
		return nil, errors.New("voice: no adapter keys configured")
	}
	return out, nil
}

type Server struct {
	voicev1.UnimplementedVoiceServer
	M    *session.Manager
	Keys Keys
}

// adapter returns the namespace the call's key belongs to.
func (s *Server) adapter(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("x-internal-key")
	if len(vals) == 0 || vals[0] == "" {
		return "", status.Error(codes.Unauthenticated, "missing adapter credentials")
	}
	found := ""
	for ns, key := range s.Keys { // compare against every key: no early exit on a match
		if subtle.ConstantTimeCompare([]byte(vals[0]), []byte(key)) == 1 {
			found = ns
		}
	}
	if found == "" {
		return "", status.Error(codes.Unauthenticated, "missing adapter credentials")
	}
	return found, nil
}

func (s *Server) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		ns, err := s.adapter(ctx)
		if err != nil {
			return nil, err
		}
		return h(context.WithValue(ctx, nsKey{}, ns), req)
	}
}

func (s *Server) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		ns, err := s.adapter(ss.Context())
		if err != nil {
			return err
		}
		return h(srv, &nsStream{ServerStream: ss, ctx: context.WithValue(ss.Context(), nsKey{}, ns)})
	}
}

type nsKey struct{}
type nsStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (n *nsStream) Context() context.Context { return n.ctx }

func ns(ctx context.Context) string { v, _ := ctx.Value(nsKey{}).(string); return v }

func mapErr(err error) error {
	var d session.DeniedError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &d):
		return status.Error(codes.PermissionDenied, d.Reason)
	case errors.Is(err, session.ErrNotFound):
		return status.Error(codes.NotFound, "no such session")
	case errors.Is(err, session.ErrEnded):
		return status.Error(codes.FailedPrecondition, "session ended")
	case errors.Is(err, session.ErrFull):
		return status.Error(codes.ResourceExhausted, "session is full")
	case errors.Is(err, session.ErrQuota):
		return status.Error(codes.ResourceExhausted, "quota exceeded")
	case errors.Is(err, session.ErrNotOwner), errors.Is(err, session.ErrForeign):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, session.ErrNotParticipant):
		return status.Error(codes.NotFound, "not a participant")
	case errors.Is(err, session.ErrConflict):
		return status.Error(codes.AlreadyExists, "native reference already in use")
	case errors.Is(err, session.ErrInvalid):
		return status.Error(codes.InvalidArgument, "invalid request")
	case errors.Is(err, session.ErrUnavailable):
		return status.Error(codes.Unavailable, "authorization unavailable")
	}
	return status.Error(codes.Internal, "voice service error")
}

func (s *Server) CreateSession(ctx context.Context, r *voicev1.CreateSessionRequest) (*voicev1.CreateSessionResponse, error) {
	out, dup, err := s.M.Create(session.CreateParams{
		Adapter: ns(ctx), Owner: r.GetOwnerAccount(), Kind: r.GetKind(), Max: r.GetMaxParticipants(),
		EndOnOwnerLeft: r.GetEndWhenOwnerLeaves(), Title: r.GetTitle(), Transport: r.GetTransport(),
		Codecs: r.GetCodecs(), IdempotencyKey: r.GetIdempotencyKey(), NativeRef: r.GetNativeRef(),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return &voicev1.CreateSessionResponse{Session: out, Duplicate: dup}, nil
}

func (s *Server) GetSession(_ context.Context, r *voicev1.GetSessionRequest) (*voicev1.GetSessionResponse, error) {
	out, err := s.M.Get(r.GetSessionId())
	if err != nil {
		return nil, mapErr(err)
	}
	return &voicev1.GetSessionResponse{Session: out}, nil
}

func (s *Server) GetSessionByNativeRef(_ context.Context, r *voicev1.GetSessionByNativeRefRequest) (*voicev1.GetSessionResponse, error) {
	out, err := s.M.ByNativeRef(r.GetNamespace(), r.GetNativeRef())
	if err != nil {
		return nil, mapErr(err)
	}
	return &voicev1.GetSessionResponse{Session: out}, nil
}

func (s *Server) ListSessions(_ context.Context, r *voicev1.ListSessionsRequest) (*voicev1.ListSessionsResponse, error) {
	if r.GetAccountId() == "" {
		return nil, mapErr(session.ErrInvalid)
	}
	return &voicev1.ListSessionsResponse{Sessions: s.M.List(r.GetAccountId())}, nil
}

func (s *Server) JoinSession(ctx context.Context, r *voicev1.JoinSessionRequest) (*voicev1.JoinSessionResponse, error) {
	out, renewed, err := s.M.Join(ctx, session.JoinParams{
		Adapter: ns(ctx), SessionID: r.GetSessionId(), Account: r.GetAccountId(),
		Transport: r.GetTransport(), Codecs: r.GetCodecs(), LeaseSec: r.GetLeaseSeconds(),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return &voicev1.JoinSessionResponse{Session: out, Renewed: renewed}, nil
}

func (s *Server) Heartbeat(ctx context.Context, r *voicev1.HeartbeatRequest) (*voicev1.HeartbeatResponse, error) {
	exp, err := s.M.Heartbeat(ns(ctx), r.GetSessionId(), r.GetAccountId(), r.GetLeaseSeconds())
	if err != nil {
		return nil, mapErr(err)
	}
	return &voicev1.HeartbeatResponse{LeaseExpiresUnix: exp.Unix()}, nil
}

func (s *Server) LeaveSession(ctx context.Context, r *voicev1.LeaveSessionRequest) (*voicev1.LeaveSessionResponse, error) {
	if err := s.M.Leave(ns(ctx), r.GetSessionId(), r.GetAccountId()); err != nil {
		return nil, mapErr(err)
	}
	return &voicev1.LeaveSessionResponse{}, nil
}

func (s *Server) EndSession(_ context.Context, r *voicev1.EndSessionRequest) (*voicev1.EndSessionResponse, error) {
	if err := s.M.End(r.GetSessionId(), r.GetActorAccount()); err != nil {
		return nil, mapErr(err)
	}
	return &voicev1.EndSessionResponse{}, nil
}

func (s *Server) Kick(_ context.Context, r *voicev1.KickRequest) (*voicev1.KickResponse, error) {
	if err := s.M.Kick(r.GetSessionId(), r.GetActorAccount(), r.GetAccountId()); err != nil {
		return nil, mapErr(err)
	}
	return &voicev1.KickResponse{}, nil
}

func (s *Server) SetMuted(ctx context.Context, r *voicev1.SetMutedRequest) (*voicev1.SetMutedResponse, error) {
	if err := s.M.SetMuted(ns(ctx), r.GetSessionId(), r.GetAccountId(), r.GetMuted()); err != nil {
		return nil, mapErr(err)
	}
	return &voicev1.SetMutedResponse{}, nil
}

func (s *Server) Invite(ctx context.Context, r *voicev1.InviteRequest) (*voicev1.InviteResponse, error) {
	dup, err := s.M.Invite(ctx, r.GetSessionId(), r.GetActorAccount(), r.GetTargetAccount(), time.Duration(r.GetTtlSeconds())*time.Second)
	if err != nil {
		return nil, mapErr(err)
	}
	return &voicev1.InviteResponse{Duplicate: dup}, nil
}

func (s *Server) SetNativeRef(ctx context.Context, r *voicev1.SetNativeRefRequest) (*voicev1.SetNativeRefResponse, error) {
	if r.GetNamespace() != ns(ctx) {
		return nil, status.Error(codes.PermissionDenied, "an adapter may only set references in its own namespace")
	}
	if err := s.M.SetNativeRef(r.GetSessionId(), r.GetNamespace(), r.GetNativeRef()); err != nil {
		return nil, mapErr(err)
	}
	return &voicev1.SetNativeRefResponse{}, nil
}

func (s *Server) WatchSession(r *voicev1.WatchSessionRequest, st grpc.ServerStreamingServer[voicev1.SessionEvent]) error {
	ch, cancel, err := s.M.Watch(r.GetSessionId())
	if err != nil {
		return mapErr(err)
	}
	defer cancel()
	for {
		select {
		case <-st.Context().Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if err := st.Send(ev); err != nil {
				return err
			}
		}
	}
}
