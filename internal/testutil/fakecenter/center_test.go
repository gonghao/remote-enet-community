package fakecenter

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"remote-enet/internal/center"
	"remote-enet/internal/identity"
	"remote-enet/internal/ticket"
)

type fixture struct {
	t               *testing.T
	c               *Center
	op, client, hub ed25519.PrivateKey
	node, account   string
	time            time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	now := time.Unix(1800000000, 0)
	f := &fixture{t: t, time: now}
	f.c = New(t, Options{Entries: 2, Now: func() time.Time { return now }, TicketBudget: 1})
	for _, k := range []*ed25519.PrivateKey{&f.op, &f.client, &f.hub} {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		*k = priv
	}
	f.node = f.c.RegisterNode(NodeConfig{FP: fp(f.hub), Addr: "127.0.0.1", Port: 9876, Region: "test", Capacity: 4})
	_, f.account = f.c.CreateAccount("garage", 1, time.Time{})
	return f
}
func fp(k ed25519.PrivateKey) identity.Fingerprint {
	return identity.FingerprintOf(k.Public().(ed25519.PublicKey))
}
func (f *fixture) request(entry int, role, path, key, nonce string, b object) *http.Request {
	f.t.Helper()
	method := "GET"
	var data []byte
	if b != nil {
		method = "POST"
		data, _ = json.Marshal(b)
	}
	r, err := http.NewRequestWithContext(f.t.Context(), method, f.c.Entries()[entry]+path, bytes.NewReader(data))
	if err != nil {
		f.t.Fatal(err)
	}
	req := center.Request{CenterID: "test-center", Method: method, Target: path, Body: data}
	auth := center.RequestAuth{Timestamp: f.time.Unix(), Client: role + "/1.0.0", Nonce: raw(nonce), IdempotencyKey: key}
	if role == "hub" {
		r.Header, err = center.SignNode(req, auth, f.hub, [16]byte(raw(f.node)))
	} else {
		k := f.op
		if role == "client" {
			k = f.client
		}
		r.Header, err = center.SignDevice(req, auth, k)
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}
func (f *fixture) send(r *http.Request, status int, code string) object {
	f.t.Helper()
	resp, err := f.c.HTTPClient().Do(r)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	var out object
	if resp.StatusCode != status || json.Unmarshal(b, &out) != nil {
		f.t.Fatalf("%s got %d %s, want %d", r.URL.Path, resp.StatusCode, b, status)
	}
	if code != "" {
		if e, ok := out["error"].(map[string]any); !ok || e["code"] != code {
			f.t.Fatalf("got %s, want %s", b, code)
		}
	}
	if resp.Header.Get("Idempotency-Replayed") == "true" {
		out["replayed"] = true
	}
	return out
}
func (f *fixture) activate() object {
	kid := ticket.KidOf(f.c.TicketKey())
	return object{"node_pub": encoding.EncodeToString(f.hub.Public().(ed25519.PublicKey)), "version": "1.0.0", "kids": []string{encoding.EncodeToString(kid[:])}}
}
func (f *fixture) setup() {
	f.t.Helper()
	f.send(f.request(0, "hub", "/v1/node/activate", randomID(), "", f.activate()), 200, "")
	f.send(f.request(0, "operator", "/v1/operator/login", randomID(), "", object{"account_code": f.account, "salt": randomID()}), 200, "")
}
func TestSignedLifecycle(t *testing.T) {
	f := newFixture(t)
	f.setup()
	path := "/v1/sessions"
	key := randomID()
	create := object{"node_id": f.node}
	f.c.Inject(0, path, Fault{DropResponse: true})
	if resp, err := f.c.HTTPClient().Do(f.request(0, "operator", path, key, randomID(), create)); err == nil {
		resp.Body.Close()
		t.Fatal("expected dropped response")
	}
	nonce := randomID()
	open := f.send(f.request(1, "operator", path, key, nonce, create), 201, "")
	if open["replayed"] != true || open["state"] != "open" || len(f.c.Snapshot()) != 1 {
		t.Fatal("create replay lost committed session")
	}
	id := open["session_id"].(string)
	sessionPath := path + "/" + id
	f.send(f.request(0, "operator", sessionPath, "", nonce, nil), 409, "nonce_reused")
	if b := f.send(f.request(1, "operator", sessionPath, "", "", nil), 200, ""); b["code"] != nil {
		t.Fatal("code without nonce")
	}
	f.send(f.request(1, "operator", path, key, "", object{"node_id": randomID()}), 422, "idempotency_key_reused")
	f.send(f.request(0, "operator", sessionPath+"/tickets", randomID(), "", object{}), 409, "session_not_ready")
	waitReq := f.request(0, "operator", sessionPath+"?since_version=1&wait_s=10", "", "", nil)
	waited := make(chan *http.Response, 1)
	go func() { resp, _ := f.c.HTTPClient().Do(waitReq); waited <- resp }()
	redeem := object{"code": open["code"], "salt": randomID()}
	result := f.send(f.request(0, "client", "/v1/redeem", randomID(), "", redeem), 200, "")
	if result["state"] != "authorized" || result["expires_at"] != nil {
		t.Fatal(result)
	}
	select {
	case resp := <-waited:
		if resp == nil {
			t.Fatal("poll failed")
		}
		defer resp.Body.Close()
		var view object
		if err := json.NewDecoder(resp.Body).Decode(&view); err != nil || view["state"] != "authorized" || view["code"] != nil {
			t.Fatal(view, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("poll did not return")
	}
	f.send(f.request(0, "client", "/v1/redeem", randomID(), "", redeem), 404, "code_invalid")
	replay := f.send(f.request(1, "operator", path, key, randomID(), create), 201, "")
	if replay["code"] != nil || replay["state"] != "authorized" {
		t.Fatal(replay)
	}
	ticketKey := randomID()
	getTicket := func(entry int, k string) *http.Request {
		return f.request(entry, "operator", sessionPath+"/tickets", k, "", object{})
	}
	issued := f.send(getTicket(0, ticketKey), 200, "")
	encoded := raw(issued["ticket"].(string))
	if len(encoded) > 512 {
		t.Fatal("ticket too large")
	}
	_, err := ticket.VerifyCenter(encoded, func(k ticket.Kid) (ed25519.PublicKey, bool) { p := f.c.TicketKey(); return p, k == ticket.KidOf(p) }, ticket.CenterExpect{Center: "test-center", Node: [16]byte(raw(f.node)), Hub: fp(f.hub), Generation: 1, TLSDevice: fp(f.op), Target: fp(f.client), Role: 2, ClockSkew: 30, TicketMaxTTL: 300}, f.time)
	if err != nil {
		t.Fatal(err)
	}
	if b := f.send(getTicket(1, ticketKey), 200, ""); b["ticket"] != issued["ticket"] {
		t.Fatal("ticket changed")
	}
	f.send(getTicket(1, randomID()), 429, "rate_limited")
	establish := object{"client_fp": fp(f.client).String()}
	f.send(f.request(0, "client", sessionPath+"/established", randomID(), "", establish), 403, "forbidden")
	if b := f.send(f.request(1, "operator", sessionPath+"/established", randomID(), "", establish), 200, ""); b["state"] != "active" {
		t.Fatal(b)
	}
	beat := object{"version": "1.0.0", "gen": 1, "seq": 1, "sessions": []object{{"state": "paired", "session_id": id, "rx_bytes": 7}}}
	heartbeat := func() *http.Request { return f.request(0, "hub", "/v1/node/heartbeat", "", "", beat) }
	if b := f.send(heartbeat(), 200, ""); len(b["renew"].([]any)) != 1 {
		t.Fatal(b)
	}
	f.send(heartbeat(), 400, "bad_request")
	f.send(f.request(0, "client", sessionPath+"/end", randomID(), "", object{"reason": "client_ended"}), 200, "")
	next := randomID()
	f.send(f.request(0, "operator", path, next, "", create), 409, "quota_releasing")
	beat["seq"] = 2
	beat["sessions"] = nil
	if b := f.send(heartbeat(), 200, ""); len(b["revoke"].([]any)) != 1 {
		t.Fatal(b)
	}
	beat["seq"] = 3
	beat["closed"] = []object{{"session_id": id, "rx_bytes": 5, "paired_s": 3}}
	f.send(heartbeat(), 200, "")
	s := f.c.Snapshot()[id]
	if !s.Released || s.Reason != "client_ended" || s.Usage.RX != 7 || s.Usage.Paired != 3 || s.Tickets != 1 {
		t.Fatal(s)
	}
	f.send(f.request(1, "operator", path, next, "", create), 201, "")
}

func TestAuthenticationFaultsAndGeneration(t *testing.T) {
	f := newFixture(t)
	path := "/v1/node/activate"
	body := f.activate()
	key := randomID()
	r := f.request(0, "hub", path, key, "", body)
	r.Header.Set("Renet-Client", "hub/2.0.0")
	f.send(r, 401, "unauthenticated")
	f.c.Inject(0, path, Fault{Code: "unavailable"})
	f.send(f.request(0, "hub", path, key, "", body), 503, "unavailable")
	f.c.Inject(1, path, Fault{TimeOffset: 32 * time.Second})
	if b := f.send(f.request(1, "hub", path, key, "", body), 200, ""); b["server_time"] != float64(f.time.Unix()+32) {
		t.Fatal(b)
	}
	f.send(f.request(1, "hub", path, randomID(), "", body), 200, "")
	f.send(f.request(1, "hub", path, key, "", body), 409, "node_superseded")
	login := "/v1/operator/login"
	body = object{"salt": randomID(), "account_code": f.account}
	r = f.request(1, "operator", login, key, "", body)
	r.Body = io.NopCloser(strings.NewReader("{}"))
	r.ContentLength = 2
	f.send(r, 401, "unauthenticated")
	f.c.Inject(0, login, Fault{Code: "rate_limited", RetryAfter: 2})
	f.send(f.request(0, "operator", login, key, "", body), 429, "rate_limited")
	f.c.Inject(0, login, Fault{Code: "device_limit"})
	f.send(f.request(0, "operator", login, key, "", body), 403, "device_limit")
	if b := f.send(f.request(1, "operator", login, key, "", body), 403, "device_limit"); b["replayed"] != true {
		t.Fatal(b)
	}
	f.c.Advance(61 * time.Second)
	f.send(f.request(1, "operator", login, randomID(), "", body), 401, "clock_skew")
}

func TestLazyDeadlinesAndRevocation(t *testing.T) {
	for _, reason := range []string{"node_lost", "account_expired", "device_revoked", "node_disabled"} {
		t.Run(reason, func(t *testing.T) {
			f := newFixture(t)
			f.setup()
			b := f.send(f.request(0, "operator", "/v1/sessions", randomID(), randomID(), object{"node_id": f.node}), 201, "")
			id := b["session_id"].(string)
			switch reason {
			case "node_lost":
				f.c.Advance(45 * time.Second)
			case "account_expired":
				f.c.SetAccount(f.account, "active", 1, f.time.Add(10*time.Second))
				f.c.Advance(10 * time.Second)
			case "device_revoked":
				f.c.ResetAccountCode(f.account)
				f.send(f.request(1, "operator", "/v1/operator/account", "", "", nil), 401, "unauthenticated")
			case "node_disabled":
				f.c.SetNodeState(f.node, "disabled")
			}
			s := f.c.Snapshot()[id]
			if s.State != "ended" || s.Reason != reason || !s.Released {
				t.Fatal(s)
			}
			f.send(f.request(1, "client", "/v1/redeem", randomID(), "", object{"code": b["code"], "salt": randomID()}), 404, "code_invalid")
		})
	}
}
