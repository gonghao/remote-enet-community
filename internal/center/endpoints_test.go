package center_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"remote-enet/internal/center"
	"remote-enet/internal/identity"
	"remote-enet/internal/settings"
	"remote-enet/internal/testutil/fakecenter"
	"remote-enet/internal/ticket"
)

func TestEndpointLifecycle(t *testing.T) {
	ctx := t.Context()
	fc := fakecenter.New(t, fakecenter.Options{Entries: 2})
	var node center.NodeID
	newClient := func(role string) (*center.Client, identity.Fingerprint) {
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		fp := identity.FingerprintOf(pub)
		opts := center.ClientOptions{Center: settings.Center{ID: "test-center", API: fc.Entries()}, Role: role, AppVersion: "1.0.0", Device: priv, HTTP: fc.HTTPClient()}
		if role == "hub" {
			id := fc.RegisterNode(fakecenter.NodeConfig{FP: fp, Addr: "127.0.0.1", Port: 9200, Region: "test", Capacity: 2})
			if err := node.UnmarshalText([]byte(id)); err != nil {
				t.Fatal(err)
			}
			opts.Device = nil
			opts.NodeKey = priv
			opts.Node = node
		}
		c, err := center.NewClient(opts)
		if err != nil {
			t.Fatal(err)
		}
		return c, fp
	}
	hub, _ := newClient("hub")
	op, operatorFP := newClient("operator")
	client, clientFP := newClient("client")
	key := center.NewIdempotencyKey
	activation := center.Activation{Version: "1.0.0", Kids: []center.KeyID{center.KeyID(ticket.KidOf(fc.TicketKey()))}}
	activated, err := hub.Activate(ctx, key(), activation)
	if err != nil || activated.Gen == 0 {
		t.Fatal(activated, err)
	}
	_, accountCode := fc.CreateAccount("test", 1, time.Time{})
	loginKey := key()
	for range 2 {
		device, acct, err := op.Login(ctx, loginKey, accountCode, "operator")
		if err != nil || device.DeviceFP != operatorFP || acct.Quota != 1 {
			t.Fatal("login", err)
		}
	}
	_, _, err = op.Login(ctx, loginKey, accountCode, "different")
	wantCode(t, err, "idempotency_key_reused")
	devices, err := op.Devices(ctx)
	if err != nil || len(devices) != 1 || !devices[0].Current {
		t.Fatal(devices, err)
	}
	nodes, err := op.Nodes(ctx)
	if err != nil || len(nodes) != 1 || nodes[0].NodeID != node {
		t.Fatal(nodes, err)
	}
	fc.Inject(0, "/v1/sessions", fakecenter.Fault{DropResponse: true})
	s, err := op.CreateSession(ctx, key(), node)
	if err != nil || s.Code == nil || fc.Calls(1, "/v1/sessions") != 1 || len(fc.Snapshot()) != 1 {
		t.Fatal("create replay", err)
	}
	id := s.SessionID
	_, err = op.CreateSession(ctx, key(), node)
	wantCode(t, err, "device_busy")
	acct, err := op.Account(ctx)
	if err != nil || len(acct.Sessions) != 1 || acct.Sessions[0].SessionID != id {
		t.Fatal(acct, err)
	}
	_, err = client.Session(ctx, id, 0, 0, false)
	wantCode(t, err, "not_found")
	short, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	_, err = op.Ticket(short, key(), id)
	stop()
	wantCode(t, err, "session_not_ready")
	path := "/v1/sessions/" + id.String()
	fc.Inject(0, path, fakecenter.Fault{Code: "nonce_reused"})
	read, err := op.Session(ctx, id, 0, 0, true)
	if err != nil || read.Code == nil || *read.Code != *s.Code {
		t.Fatal("read with fresh nonce", err)
	}
	type pollResult struct {
		s   center.Session
		err error
	}
	done := make(chan pollResult, 1)
	beforePoll := fc.Calls(0, path)
	go func() { s, err := op.Session(ctx, id, s.Version, 5*time.Second, false); done <- pollResult{s, err} }()
	deadline := time.Now().Add(time.Second)
	for fc.Calls(0, path) == beforePoll {
		if time.Now().After(deadline) {
			t.Fatal("long poll did not arrive")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("long poll returned before redemption")
	default:
	}
	redeemKey := key()
	for range 2 {
		authorized, err := client.Redeem(ctx, redeemKey, *s.Code)
		if err != nil || authorized.State != "authorized" || authorized.ExpiresAt != nil {
			t.Fatal("redeem", err)
		}
	}
	result := <-done
	if result.err != nil || result.s.State != "authorized" {
		t.Fatal(result.err, result.s.State)
	}
	_, err = client.Redeem(ctx, key(), *s.Code)
	wantCode(t, err, "code_invalid")
	sessions, err := client.ClientSessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].SessionID != id {
		t.Fatal(sessions, err)
	}
	for _, c := range []*center.Client{op, client} {
		ticket, err := c.Ticket(ctx, key(), id)
		if err != nil || len(ticket.Raw) == 0 {
			t.Fatal("ticket", err)
		}
	}
	active, err := op.Established(ctx, key(), id, clientFP)
	if err != nil || active.State != "active" {
		t.Fatal(active, err)
	}
	usage := center.ClosedUsage{SessionID: id, RxBytes: 100, TxBytes: 200, PairedS: 10}
	hb := center.Heartbeat{Gen: activated.Gen, Seq: 1, Version: activation.Version, Kids: activation.Kids, Load: center.Load{Conns: 2, Sessions: 1}, Sessions: []center.SessionUsage{{ClosedUsage: usage, State: "paired"}}}
	reply, err := hub.Heartbeat(ctx, hb)
	if err != nil || len(reply.Renew) != 1 || reply.Renew[0].SessionID != id {
		t.Fatal(reply, err)
	}
	ended, err := op.End(ctx, key(), id, "operator_ended")
	if err != nil || ended.EndReason == nil || *ended.EndReason != "operator_ended" {
		t.Fatal(ended, err)
	}
	short, stop = context.WithTimeout(ctx, 500*time.Millisecond)
	_, err = op.CreateSession(short, key(), node)
	stop()
	wantCode(t, err, "quota_releasing")
	hb.Seq++
	hb.Sessions = nil
	reply, err = hub.Heartbeat(ctx, hb)
	if err != nil || len(reply.Revoke) != 1 || reply.Revoke[0].SessionID != id || reply.Revoke[0].Reason != *ended.EndReason {
		t.Fatal(reply, err)
	}
	usage.TxBytes++
	hb.Seq++
	hb.Closed = []center.ClosedUsage{usage}
	reply, err = hub.Heartbeat(ctx, hb)
	state := fc.Snapshot()[id.String()]
	if err != nil || len(reply.Revoke) != 0 || !state.Released || state.Usage.TX != usage.TxBytes {
		t.Fatal(reply, state, err)
	}
	if _, err := hub.Activate(ctx, key(), activation); err != nil {
		t.Fatal(err)
	}
	hb.Seq++
	_, err = hub.Heartbeat(ctx, hb)
	wantCode(t, err, "node_superseded")
	logoutKey := key()
	for range 2 {
		if err := op.RevokeDevice(ctx, logoutKey, operatorFP); err != nil {
			t.Fatal(err)
		}
	}
	_, err = op.Account(ctx)
	wantCode(t, err, "unauthenticated")
}
func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var api *center.Error
	if !errors.As(err, &api) || api.Code != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}
