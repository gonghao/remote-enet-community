package operator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"remote-enet/internal/hsfz"
	"remote-enet/internal/hsfzcapture"
)

type captureEvent struct {
	Type       string
	Connection uint64
	Metadata   *hsfzcapture.Metadata
	Direction  hsfzcapture.Direction
	Data       string
	Frame      *hsfzcapture.Frame
}

func captureEvents(t *testing.T, path string) []captureEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []captureEvent
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		var e captureEvent
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
func captureExchange(t *testing.T, c *net.TCPConn, payload []byte) {
	t.Helper()
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("echo: %x %v", got, err)
	}
}
func captureFrame(t *testing.T, control uint16) []byte {
	t.Helper()
	b, err := (hsfz.Frame{Control: hsfz.Control(control), Body: []byte{0xf4, 0x10, 0, 0xff}}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestCapturePresenterLifecycleAndDiscovery(t *testing.T) {
	recorder := hsfzcapture.New(hsfzcapture.Options{})
	opener, requests, _ := newEchoOpener(t)
	var gen atomic.Uint64
	gen.Store(11)
	var current atomic.Pointer[Presenter]
	p := startPresenter(t, Options{Opener: opener, State: testState(t, "127.0.0.2"), Capture: recorder, CaptureGeneration: func() uint64 {
		// Callbacks may inspect presenter state and must never run under its lock.
		_ = current.Load().snapshot()
		return gen.Load()
	}, RemoteDiagnosticPort: 6801, RemoteControlPort: 6801})
	current.Store(p)
	diag, ctrl, disc := p.Addr()
	old := dialTCP(t, diag)
	nextRequest(t, requests)
	first, second := filepath.Join(t.TempDir(), "first.jsonl"), filepath.Join(t.TempDir(), "second.jsonl")
	if err := recorder.Start(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	payload := captureFrame(t, 0x9988)
	captureExchange(t, old, payload)
	for _, addr := range []*net.TCPAddr{diag, ctrl} {
		c := dialTCP(t, addr)
		nextRequest(t, requests)
		captureExchange(t, c, payload)
	}
	gen.Store(12)
	state := testState(t, "127.0.0.3")
	if err := p.Update(state); err != nil {
		t.Fatal(err)
	}
	c := udpClient(t)
	probe(t, c, disc, hsfz.IdentRequest, state.IdentRaw)
	held := dialTCP(t, diag)
	nextRequest(t, requests)
	if err := recorder.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Start(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	captureExchange(t, held, payload)
	fresh := dialTCP(t, ctrl)
	nextRequest(t, requests)
	captureExchange(t, fresh, payload)
	p.Close()
	if err := recorder.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	firstEvents := captureEvents(t, first)
	var starts, frames int
	channels := map[string]int{}
	data := map[hsfzcapture.Direction][]byte{}
	for _, e := range firstEvents {
		if e.Type == "connection_start" {
			starts++
			channels[e.Metadata.Channel]++
			if e.Metadata.Generation != 11 && e.Metadata.Generation != 12 {
				t.Fatalf("generation: %+v", e.Metadata)
			}
			if e.Metadata.Local == "" || e.Metadata.Peer == "" || e.Metadata.Target == "" {
				t.Fatalf("metadata: %+v", e.Metadata)
			}
		}
		if e.Type == "data" {
			b, err := hex.DecodeString(e.Data)
			if err != nil {
				t.Fatal(err)
			}
			data[e.Direction] = append(data[e.Direction], b...)
		}
		if e.Type == "frame" {
			frames++
		}
	}
	if starts != 5 || channels["diagnostic"] != 2 || channels["control"] != 1 || channels["discovery"] != 2 || frames != 6 {
		t.Fatalf("starts=%d channels=%v frames=%d", starts, channels, frames)
	}
	twice := append(bytes.Clone(payload), payload...)
	if !bytes.Equal(data[hsfzcapture.ToVehicle], twice) || !bytes.Equal(data[hsfzcapture.FromVehicle], twice) || !bytes.Equal(data[hsfzcapture.ToOperator], hsfz.IdentRequest) || !bytes.Equal(data[hsfzcapture.FromOperator], state.IdentRaw) {
		t.Fatalf("captured bytes: %x", data)
	}
	starts = 0
	for _, e := range captureEvents(t, second) {
		if e.Type == "connection_start" {
			starts++
			if e.Metadata.Channel != "control" || e.Metadata.Generation != 12 || e.Metadata.Target != "127.0.0.3:6801" {
				t.Fatalf("new metadata: %+v", e.Metadata)
			}
		}
	}
	if starts != 1 {
		t.Fatalf("old handles attached to new capture: %d", starts)
	}
}

func TestCaptureGateRejectsBeforeBegin(t *testing.T) {
	recorder := hsfzcapture.New(hsfzcapture.Options{})
	path := filepath.Join(t.TempDir(), "gate.jsonl")
	if err := recorder.Start(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	opener, requests, _ := newEchoOpener(t)
	resolver, queries := controlledResolver()
	p := startPresenter(t, Options{Opener: opener, State: testState(t, "127.0.0.2"), Capture: recorder, Exclusive: true, Resolver: resolver})
	diag, _, _ := p.Addr()
	c := dialTCP(t, diag)
	q := nextOwnerQuery(t, queries)
	q.reply <- resolvedOwner{err: errors.New("denied")}
	expectRejected(t, c)
	p.Close()
	noTunnelRequest(t, requests)
	if err := recorder.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, e := range captureEvents(t, path) {
		if e.Type == "connection_start" || e.Type == "data" {
			t.Fatalf("rejected connection recorded: %+v", e)
		}
	}
}

func TestCaptureLimitKeepsForwarding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options hsfzcapture.Options
	}{
		{name: "file", options: hsfzcapture.Options{MaxFileBytes: 1024}},
		{name: "queue", options: hsfzcapture.Options{QueueBytes: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := hsfzcapture.New(tc.options)
			path := filepath.Join(t.TempDir(), "limit.jsonl")
			if err := recorder.Start(t.Context(), path); err != nil {
				t.Fatal(err)
			}
			opener, requests, _ := newEchoOpener(t)
			p := startPresenter(t, Options{Opener: opener, State: testState(t, "127.0.0.2"), Capture: recorder})
			diag, _, _ := p.Addr()
			for range 3 {
				c := dialTCP(t, diag)
				nextRequest(t, requests)
				captureExchange(t, c, bytes.Repeat(captureFrame(t, 1), 1024))
			}
			p.Close()
			if err := recorder.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			snapshot := recorder.Snapshot()
			if !snapshot.Incomplete {
				t.Fatal("limit did not mark incomplete capture")
			}
			if tc.name == "queue" && snapshot.DroppedEvents == 0 {
				t.Fatal("queue rejection was not recorded")
			}
		})
	}
}
