// Voice: the neutral voice session service. It owns who is in which voice
// session and who may join (the account core's friendship/block/status answer
// at the door), and announces invitations on the core's event stream. Platform
// adapters (xb-voice first) speak their consoles' native protocols and call it
// over gRPC. The media plane is not implemented yet: see docs/media-plane.md.
// Ports (block 20160 of ports.md): 20160 HTTP (/healthz, /metrics), 20161 gRPC,
// 20162 reserved for metrics.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/openpak/voice/internal/core"
	"github.com/openpak/voice/internal/grpcapi"
	"github.com/openpak/voice/internal/session"
	voicev1 "github.com/openpak/voice/proto/openpak/voice/v1"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// secret reads NAME, or the file named by NAME_FILE (preferred: nothing secret
// in the unit's environment).
func secret(name string) (string, error) {
	if f := os.Getenv(name + "_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return os.Getenv(name), nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	coreKey, err := secret("VOICE_CORE_INTERNAL_KEY")
	if err != nil || coreKey == "" {
		return fmt.Errorf("voice: the account core credential is required (VOICE_CORE_INTERNAL_KEY or _FILE): %v", err)
	}
	raw, err := secret("VOICE_ADAPTER_KEYS")
	if err != nil {
		return err
	}
	keys, err := grpcapi.ParseKeys(strings.ReplaceAll(raw, "\n", ","))
	if err != nil {
		return err
	}
	cc, err := core.Dial(env("VOICE_CORE_ADDRESS", "127.0.0.1:20001"), coreKey)
	if err != nil {
		return err
	}
	m := &session.Manager{Auth: cc, Announce: cc}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go m.Run(ctx, 5*time.Second)

	api := &grpcapi.Server{M: m, Keys: keys}
	lis, err := net.Listen("tcp", env("VOICE_GRPC_ADDR", "127.0.0.1:20161"))
	if err != nil {
		return err
	}
	gs := grpc.NewServer(grpc.UnaryInterceptor(api.Unary()), grpc.StreamInterceptor(api.Stream()))
	voicev1.RegisterVoiceServer(gs, api)
	go func() {
		log.Printf("voice: grpc on %s (adapters: %d)", lis.Addr(), len(keys))
		if err := gs.Serve(lis); err != nil {
			log.Printf("voice: grpc: %v", err)
			stop()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		s, p := m.Counts()
		fmt.Fprintf(w, "voice_sessions_live %d\nvoice_participants_live %d\n", s, p)
	})
	hs := &http.Server{Addr: env("VOICE_HTTP_ADDR", "127.0.0.1:20160"), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("voice: http on %s", hs.Addr)
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("voice: http: %v", err)
			stop()
		}
	}()

	<-ctx.Done()
	gs.GracefulStop()
	sd, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(sd)
}
