package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"remote-enet/internal/hsfz"
	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/proto"
	"remote-enet/internal/transport"
)

type queryCaptureEvent struct {
	Type       string
	Connection uint64
	Metadata   *hsfzcapture.Metadata
	Direction  hsfzcapture.Direction
	Data       string
	Frame      *hsfzcapture.Frame
}

func queryCaptureEvents(t *testing.T, path string) []queryCaptureEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []queryCaptureEvent
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		var e queryCaptureEvent
		if err := json.Unmarshal(scan.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}
func TestCaptureInternalMGUAndPresenterSession(t *testing.T) {
	recorder := hsfzcapture.New(hsfzcapture.Options{})
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := recorder.Start(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	device := memoryDirectDevice(t)
	f := newDirectFixture(t, OperatorConfig{Capture: recorder, DirectFactory: func(context.Context, []netip.Addr) (DirectDevice, error) { return device, nil }}, mguReply)
	generation := f.r.Snapshot().Generation
	if err := f.r.ConnectMGU(t.Context(), generation); err != nil {
		t.Fatal(err)
	}
	if err := f.r.DisconnectMGU(t.Context(), generation); err != nil {
		t.Fatal(err)
	}
	assertDirectHSFZ(t, f)
	awaitMetrics(t, f.r, func(Snapshot) bool { return recorder.Snapshot().ObservedBytes == 70 })
	// Presenter owns only forwarding sockets; Runtime never stops the recorder.
	f.cancel()
	ended := <-f.done
	f.done <- ended
	if !recorder.Snapshot().Active {
		t.Fatal("runtime stopped global recorder")
	}
	if err := recorder.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	events := queryCaptureEvents(t, path)
	metadata := map[uint64]hsfzcapture.Metadata{}
	payloads := map[string]map[hsfzcapture.Direction][]byte{}
	var frames int
	for _, e := range events {
		if e.Type == "connection_start" {
			if e.Metadata.Generation != generation || e.Metadata.Network != "tcp" || e.Metadata.Local == "" || e.Metadata.Peer == "" || e.Metadata.Target == "" {
				t.Fatalf("metadata: %+v", e.Metadata)
			}
			metadata[e.Connection] = *e.Metadata
			payloads[e.Metadata.Channel] = map[hsfzcapture.Direction][]byte{}
		}
		if e.Type == "data" {
			b, err := hex.DecodeString(e.Data)
			if err != nil {
				t.Fatal(err)
			}
			channel := metadata[e.Connection].Channel
			payloads[channel][e.Direction] = append(payloads[channel][e.Direction], b...)
		}
		if e.Type == "frame" {
			frames++
		}
	}
	request, _ := (hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x63, 0x22, 0x17, 0x2a}}).MarshalBinary()
	reply, _ := (hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0x63, 0xf4, 0x62, 0x17, 0x2a, 0, 10, 99, 0, 8, 255, 255, 255, 0, 10, 99, 0, 1}}).MarshalBinary()
	if len(metadata) != 2 || len(payloads) != 2 || frames != 4 {
		t.Fatalf("connections=%v payloads=%v frames=%d", metadata, payloads, frames)
	}
	for _, channel := range []string{"mgu-query", "diagnostic"} {
		if !bytes.Equal(payloads[channel][hsfzcapture.ToVehicle], request) || !bytes.Equal(payloads[channel][hsfzcapture.FromVehicle], reply) {
			t.Fatalf("%s bytes: %x", channel, payloads[channel])
		}
	}
}

type queryOpenFunc func(context.Context, proto.OpenTunnel) (transport.Stream, error)

func (f queryOpenFunc) OpenTunnel(ctx context.Context, h proto.OpenTunnel) (transport.Stream, error) {
	return f(ctx, h)
}

type partialQueryStream struct{ closed, half int }

func (s *partialQueryStream) Read(p []byte) (int, error)  { copy(p, []byte("read")); return 4, io.EOF }
func (s *partialQueryStream) Write(p []byte) (int, error) { return 2, io.ErrShortWrite }
func (s *partialQueryStream) CloseWrite() error           { s.half++; return nil }
func (s *partialQueryStream) Close() error                { s.closed++; return nil }
func TestCaptureQueryActualBytesAndOwnership(t *testing.T) {
	recorder := hsfzcapture.New(hsfzcapture.Options{})
	path := filepath.Join(t.TempDir(), "partial.jsonl")
	if err := recorder.Start(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	raw := &partialQueryStream{}
	opener := captureQueryOpener{recorder: recorder, generation: func() uint64 { return 8 }, opener: queryOpenFunc(func(context.Context, proto.OpenTunnel) (transport.Stream, error) { return raw, nil })}
	s, err := opener.OpenTunnel(t.Context(), proto.OpenTunnel{Kind: "tcp", Target: "127.0.0.1:6801"})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	if n, err := s.Read(buf); n != 4 || !errors.Is(err, io.EOF) {
		t.Fatalf("read: %d %v", n, err)
	}
	if n, err := s.Write([]byte("four")); n != 2 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("write: %d %v", n, err)
	}
	if err := s.CloseWrite(); err != nil || raw.closed != 0 || raw.half != 1 {
		t.Fatalf("half-close: %v %+v", err, raw)
	}
	s.Close()
	s.Close()
	if raw.closed != 1 {
		t.Fatal("stream close ownership changed")
	}
	if err := recorder.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	data := map[hsfzcapture.Direction]string{}
	for _, e := range queryCaptureEvents(t, path) {
		if e.Type == "data" {
			b, err := hex.DecodeString(e.Data)
			if err != nil {
				t.Fatal(err)
			}
			data[e.Direction] += string(b)
		}
	}
	if data[hsfzcapture.ToVehicle] != "fo" || data[hsfzcapture.FromVehicle] != "read" {
		t.Fatalf("actual bytes: %v", data)
	}
}
func TestCaptureQueryCancellationJoinsRead(t *testing.T) {
	recorder := hsfzcapture.New(hsfzcapture.Options{})
	if err := recorder.Start(t.Context(), filepath.Join(t.TempDir(), "cancel.jsonl")); err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer b.Close()
	started := make(chan struct{})
	raw := &queryPipeStream{Conn: a, started: started}
	opener := captureQueryOpener{recorder: recorder, opener: queryOpenFunc(func(context.Context, proto.OpenTunnel) (transport.Stream, error) { return raw, nil })}
	s, err := opener.OpenTunnel(t.Context(), proto.OpenTunnel{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = s.Read(make([]byte, 1)) }()
	<-started
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	if recorder.Snapshot().ActiveConnections != 0 {
		t.Fatal("close returned before capture end")
	}
	if err := recorder.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}

type queryPipeStream struct {
	net.Conn
	started chan struct{}
}

func (s *queryPipeStream) Read(p []byte) (int, error) { close(s.started); return s.Conn.Read(p) }
func (s *queryPipeStream) CloseWrite() error          { return s.Conn.Close() }

func TestCaptureQueryDisabledAndOldEpoch(t *testing.T) {
	recorder := hsfzcapture.New(hsfzcapture.Options{})
	raw := &partialQueryStream{}
	opener := captureQueryOpener{recorder: recorder, opener: queryOpenFunc(func(context.Context, proto.OpenTunnel) (transport.Stream, error) { return raw, nil })}
	disabled, err := opener.OpenTunnel(t.Context(), proto.OpenTunnel{})
	if err != nil || disabled != raw {
		t.Fatalf("disabled stream changed: %T %v", disabled, err)
	}
	first, second := filepath.Join(t.TempDir(), "old.jsonl"), filepath.Join(t.TempDir(), "new.jsonl")
	if err := recorder.Start(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	old, err := opener.OpenTunnel(t.Context(), proto.OpenTunnel{Target: "127.0.0.1:6801"})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Start(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	_, _ = disabled.Write([]byte("disabled"))
	_, _ = old.Write([]byte("old"))
	_, _ = old.Read(make([]byte, 4))
	_ = old.Close()
	if err := recorder.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, e := range queryCaptureEvents(t, second) {
		if e.Type == "connection_start" || e.Type == "data" {
			t.Fatalf("old query entered new epoch: %+v", e)
		}
	}
}
