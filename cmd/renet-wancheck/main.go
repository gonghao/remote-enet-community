// Command renet-wancheck validates pinned peer transport without vehicle sockets.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"remote-enet/internal/carside"
	"remote-enet/internal/hsfz"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/linktest"
	"remote-enet/internal/proto"
	"remote-enet/internal/secure"
	"remote-enet/internal/session"
	"remote-enet/internal/transport"
	hubtransport "remote-enet/internal/transport/hub"
	"remote-enet/internal/trust"
)

const version = "wancheck/v1"

type phase struct {
	Name       string `json:"name"`
	Code       string `json:"code"`
	DurationMS int64  `json:"durationMs"`
	Samples    int    `json:"samples"`
	Sent       uint64 `json:"bytesSent"`
	Received   uint64 `json:"bytesReceived"`
	RTTP50NS   int64  `json:"rttP50Nanoseconds"`
	RTTP95NS   int64  `json:"rttP95Nanoseconds"`
}

type report struct {
	Schema        string  `json:"schema"`
	Success       bool    `json:"success"`
	DryRun        bool    `json:"dryRun"`
	Cleanup       bool    `json:"cleanup"`
	TimedOut      bool    `json:"timedOut"`
	Canceled      bool    `json:"canceled"`
	Code          string  `json:"code"`
	Scope         string  `json:"scope"`
	RejectedFlows uint64  `json:"rejectedTunnels"`
	Phases        []phase `json:"phases"`
}

func baseReport() report {
	return report{Schema: version, Code: "not_run", Scope: "pinned TLS/session, synthetic vehicle metadata, peer-only echo; all vehicle tunnels rejected; external network must be verified separately", Phases: []phase{}}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	cancel()
	os.Exit(code)
}

func run(ctx context.Context, args []string, input *os.File, out, diagnostics io.Writer) int {
	flags := flag.NewFlagSet("renet-wancheck", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	apply := flags.Bool("run", false, "execute the peer-only probe; invitation comes from a private regular stdin file")
	printVersion := flags.Bool("version", false, "print tool version")
	endpoint := flags.String("expected-endpoint", "", "require this exact single pinned endpoint")
	stateDir := flags.String("state-dir", "", "new private state directory; existing directories are rejected")
	timeout := flags.Duration("timeout", 2*time.Minute, "total caller budget, 30s..3m")
	hold := flags.Duration("hold", 5*time.Second, "hold each verified session for external observation, 0..15s")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *printVersion {
		fmt.Fprintln(out, version)
		return 0
	}
	if flags.NArg() != 0 || *timeout < 30*time.Second || *timeout > 3*time.Minute || *hold < 0 || *hold > 15*time.Second {
		return 2
	}
	r := baseReport()
	if !*apply {
		r.DryRun = true
	} else {
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		r = probe(ctx, input, *endpoint, *stateDir, *hold)
	}
	if err := json.NewEncoder(out).Encode(r); err != nil {
		return 1
	}
	if *apply && !r.Success {
		return 1
	}
	return 0
}

func probe(ctx context.Context, input *os.File, expected, dir string, hold time.Duration) (r report) {
	r = baseReport()
	r.Cleanup = true
	var rejected atomic.Uint64
	defer func() {
		r.RejectedFlows = rejected.Load()
		r.TimedOut, r.Canceled = errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(ctx.Err(), context.Canceled)
		if ctx.Err() != nil {
			r.Success = false
		}
		if r.RejectedFlows != 0 {
			r.Success, r.Code = false, "unexpected_vehicle_tunnel"
		}
	}()
	if ctx.Err() != nil {
		r.Code = "canceled"
		r.Canceled = true
		return r
	}
	host, port, err := net.SplitHostPort(expected)
	number, portErr := strconv.Atoi(port)
	if err != nil || host == "" || portErr != nil || number < 1 || number > 65535 || dir == "" {
		r.Code = "invalid_options"
		return r
	}
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 128<<10 {
		r.Code = "invalid_invitation_input"
		return r
	}
	raw, err := io.ReadAll(io.LimitReader(input, (128<<10)+1))
	if err != nil || len(raw) > 128<<10 {
		r.Code = "invalid_invitation_input"
		return r
	}
	i, err := invite.Decode(strings.TrimSpace(strings.TrimPrefix(string(raw), "\uFEFF")))
	if err != nil || i.Peer.ID.IsZero() || i.Token == ([16]byte{}) || len(i.Peer.Endpoints) != 1 {
		r.Code = "invalid_invitation"
		return r
	}
	ep := i.Peer.Endpoints[0]
	if ep.Kind != "hub" || ep.Addr != expected || ep.Hub != i.Peer.ID {
		r.Code = "invitation_endpoint_mismatch"
		return r
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		r.Code = "state_directory_unavailable"
		return r
	}
	store, err := trust.Open(ctx, dir)
	if err != nil {
		r.Code = "state_unavailable"
		return r
	}
	id, err := store.LoadIdentity(ctx)
	if err != nil {
		r.Code = "identity_unavailable"
		return r
	}
	peer, token := i.Peer, i.Token
	for _, name := range []string{"first_pairing", "paired_reconnect"} {
		if name == "paired_reconnect" {
			peer, err = store.Peer(ctx, i.Peer.ID)
			token = [16]byte{}
			if err != nil {
				r.Code = "paired_endpoint_unavailable"
				return r
			}
		}
		p, cleanup := runPhase(ctx, name, id, peer, token, &rejected, hold)
		r.Phases = append(r.Phases, p)
		r.Cleanup = r.Cleanup && cleanup
		if p.Code != "ok" || !cleanup {
			r.Code = p.Code
			if !cleanup {
				r.Code = "cleanup_failed"
			}
			r.TimedOut, r.Canceled = errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(ctx.Err(), context.Canceled)
			return r
		}
		if name == "first_pairing" {
			if err := store.SavePeer(ctx, peer); err != nil {
				r.Code = "paired_endpoint_save_failed"
				return r
			}
		}
	}
	r.RejectedFlows = rejected.Load()
	r.Success = r.Cleanup && r.RejectedFlows == 0
	r.Code = "ok"
	if !r.Success {
		r.Code = "unexpected_vehicle_tunnel"
	}
	return r
}

func runPhase(parent context.Context, name string, id *identity.Local, peer transport.PeerInfo, token [16]byte, rejected *atomic.Uint64, hold time.Duration) (p phase, cleanup bool) {
	p = phase{Name: name, Code: "transport_failed"}
	cleanup = true
	start := time.Now()
	defer func() { p.DurationMS = time.Since(start).Milliseconds() }()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	connector, _, err := hubtransport.New(hubtransport.Options{Local: id, Wrap: func(ctx context.Context, conn net.Conn, initiator bool, expect identity.Fingerprint, path transport.PathInfo) (transport.Session, error) {
		return secure.WrapStream(ctx, conn, secure.Opts{Local: id, IsClient: initiator, Expect: expect, Path: path})
	}})
	if err != nil {
		return
	}
	var ts transport.Session
	for {
		ts, err = connector.Dial(ctx, peer)
		if !errors.Is(err, hubtransport.ErrPeerBusy) {
			break
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	if err != nil {
		return
	}
	var s *session.Session
	defer func() {
		cancel()
		if err := ts.Close(context.Canceled); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
			cleanup = false
		}
		select {
		case <-ts.Done():
		case <-time.After(5 * time.Second):
			cleanup = false
		}
		if s != nil {
			select {
			case <-s.Done():
			case <-time.After(5 * time.Second):
				cleanup = false
			}
		}
	}()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err = session.Run(ctx, ts, session.Options{Role: session.RoleClient, Initiator: true, Token: token, AllowLinkTest: true, Logger: logger,
		OnTunnel: func(_ context.Context, _ proto.OpenTunnel, stream transport.Stream) {
			rejected.Add(1)
			_ = stream.Close()
		}})
	if err != nil {
		p.Code = "session_failed"
		return
	}
	ident := hsfz.Identification{DiagAddr: 0x10, MAC: [6]byte{1, 2, 3, 4, 5, 6}, VIN: "WBAXXXXXXX0000001"}
	data, err := (hsfz.Frame{Control: hsfz.ControlVehicleIdent, Body: ident.MarshalBody()}).MarshalBinary()
	if err != nil {
		p.Code = "fixture_failed"
		return
	}
	gateway := carside.Gateway{Address: netip.MustParseAddr("198.18.1.2"), Identification: ident, IdentRaw: data}
	nic := carside.NIC{Name: "wan-fixture", Address: netip.MustParseAddr("198.18.1.1"), Prefix: netip.MustParsePrefix("198.18.1.0/24")}
	if err := s.Send(gateway.VehicleState(nic, 1)); err != nil {
		p.Code = "fixture_publish_failed"
		return
	}
	result, err := s.TestLink(ctx, linktest.Options{Samples: 10, PayloadBytes: 1 << 20, Timeout: 15 * time.Second})
	p.Samples, p.Sent, p.Received = result.Samples, result.BytesSent, result.BytesReceived
	p.RTTP50NS, p.RTTP95NS = int64(result.RTTP50), int64(result.RTTP95)
	if err != nil {
		p.Code = "peer_echo_failed"
		return
	}
	if result.Samples != 10 || result.BytesSent != 1<<20 || result.BytesReceived != 1<<20 || result.EchoedBytes != (1<<20)-320 {
		p.Code = "peer_echo_mismatch"
		return
	}
	if hold > 0 {
		timer := time.NewTimer(hold)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			p.Code = "canceled"
			return
		case <-s.Done():
			p.Code = "peer_session_closed"
			return
		}
	}
	p.Code = "ok"
	return
}
