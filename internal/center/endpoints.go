package center

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"remote-enet/internal/centercode"
	"remote-enet/internal/identity"
)

// ErrEndpointResponse marks an invalid endpoint response shape or value.
var ErrEndpointResponse = errors.New("center: invalid endpoint response")

// Unknown v1 fields are extensions. Heartbeat decision lists may be absent or null.
func checkShape(raw jsontext.Value, typ reflect.Type) error {
	if raw.Kind() == 'n' {
		return ErrEndpointResponse
	}
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		var fields map[string]jsontext.Value
		if err := json.Unmarshal(raw, &fields); err != nil {
			return ErrEndpointResponse
		}
		for i := range typ.NumField() {
			f := typ.Field(i)
			if f.Anonymous {
				if err := checkShape(raw, f.Type); err != nil {
					return err
				}
				continue
			}
			v, ok := fields[f.Tag.Get("json")]
			if typ == reflect.TypeFor[HeartbeatResult]() && f.Type.Kind() == reflect.Slice && (!ok || v.Kind() == 'n') {
				continue
			}
			if !ok && f.Type.Kind() == reflect.Pointer {
				continue
			}
			if !ok {
				return ErrEndpointResponse
			}
			if err := checkShape(v, f.Type); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if typ.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		var items []jsontext.Value
		if err := json.Unmarshal(raw, &items); err != nil {
			return ErrEndpointResponse
		}
		for _, v := range items {
			if err := checkShape(v, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

type ticketBytes []byte

func (b *ticketBytes) UnmarshalText(text []byte) error {
	raw, err := wire64.DecodeString(string(text))
	if err != nil || len(raw) < 1 || len(raw) > 1024 || wire64.EncodeToString(raw) != string(text) {
		return ErrEndpointResponse
	}
	*b = raw
	return nil
}

var fingerprints = json.UnmarshalFunc(func(raw []byte, dst *identity.Fingerprint) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	fp, err := identity.ParseFingerprint(s)
	if err != nil || (s != fp.String() && s != strings.ReplaceAll(fp.String(), "-", "")) {
		return ErrEndpointResponse
	}
	*dst = fp
	return nil
})

func endpoint[T any](ctx context.Context, c *Client, call Call) (T, error) {
	var zero, out T
	var raw jsontext.Value
	if _, err := c.Do(ctx, call, &raw); err != nil {
		return zero, err
	}
	if err := checkShape(raw, reflect.TypeFor[T]()); err != nil {
		return zero, err
	}
	if err := json.Unmarshal(raw, &out, json.WithUnmarshalers(fingerprints)); err != nil {
		return zero, ErrEndpointResponse
	}
	return out, nil
}

func get(path string) Call { return Call{Method: "GET", Path: path} }

func post(path, key string, body any) Call {
	return Call{Method: "POST", Path: path, Key: key, Body: body}
}

// A keyed PRF gives each operation a secret random salt that survives
// restarts without storing codes or retaining a per-operation salt table.
func (c *Client) salt(path, key string) string {
	mac := hmac.New(sha256.New, c.o.Device)
	fmt.Fprintf(mac, "renet-center/v1 salt\x00%s\x00%s\x00%s", c.o.Center.ID, path, key)
	return wire64.EncodeToString(mac.Sum(nil)[:16])
}

// Login refreshes the device's absolute expiry on a new operation.
func (c *Client) Login(ctx context.Context, key, accountCode, label string) (Device, Account, error) {
	code, err := centercode.NormalizeAccountCode(accountCode)
	if err != nil {
		return Device{}, Account{}, err
	}
	if len(label) > 64 {
		return Device{}, Account{}, fmt.Errorf("%w: device label too long", ErrBadRequest)
	}
	const path = "/v1/operator/login"
	if c.o.Role != "operator" {
		return Device{}, Account{}, ErrBadRequest
	}
	body := struct {
		Code  string `json:"account_code"`
		Label string `json:"device_label,omitempty"`
		Salt  string `json:"salt"`
	}{code, label, c.salt(path, key)}
	out, err := endpoint[struct {
		Device  Device  `json:"device"`
		Account Account `json:"account"`
	}](ctx, c, post(path, key, body))
	return out.Device, out.Account, err
}

// Account returns live session summaries alongside total quota occupancy.
func (c *Client) Account(ctx context.Context) (AccountView, error) {
	return endpoint[AccountView](ctx, c, get("/v1/operator/account"))
}

// Devices identifies the signed-in record with Current.
func (c *Client) Devices(ctx context.Context) ([]DeviceEntry, error) {
	out, err := endpoint[struct {
		Devices []DeviceEntry `json:"devices"`
	}](ctx, c, get("/v1/operator/devices"))
	return out.Devices, err
}

// RevokeDevice also logs out the signer when fp is its own fingerprint.
func (c *Client) RevokeDevice(ctx context.Context, key string, fp identity.Fingerprint) error {
	_, err := c.Do(ctx, post("/v1/operator/devices/"+fp.String()+"/revoke", key, struct{}{}), nil)
	var api *Error
	if errors.As(err, &api) && api.Code == "unauthenticated" && c.o.Role == "operator" && identity.FingerprintOf(c.o.Device.Public().(ed25519.PublicKey)) == fp {
		// The signer's own record may already have been revoked by the first attempt.
		return nil
	}
	return err
}

// Nodes includes full nodes so callers can display their load.
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	out, err := endpoint[struct {
		Nodes []Node `json:"nodes"`
	}](ctx, c, get("/v1/nodes"))
	for _, node := range out.Nodes {
		if !node.valid() {
			return nil, ErrEndpointResponse
		}
	}
	return out.Nodes, err
}
func (n Node) valid() bool {
	return n.Addr != "" && n.Port != 0 && n.Load >= 0 && n.Load <= 100 && (n.State == "available" || n.State == "full")
}

func (s Session) validate(role string, nonce bool) error {
	open := s.State == "open"
	ended := s.State == "ended"
	if !open && !ended && s.State != "authorized" && s.State != "active" {
		return ErrEndpointResponse
	}
	code := open && nonce && role == "operator"
	if (s.Code != nil) != code || (s.CodeExpiresAt != nil) != code || (s.ExpiresAt != nil) != (role == "operator" && !ended) || (s.EndedAt != nil) != ended || (s.EndReason != nil) != ended {
		return ErrEndpointResponse
	}
	paired := s.ClientFP != nil
	if (paired && open) || (!paired && !open && !ended) || (s.PairDeadline != nil) != paired || (s.RedeemedAt != nil) != paired {
		return ErrEndpointResponse
	}
	if code {
		normalized, err := centercode.NormalizeNumericCode(*s.Code)
		if err != nil || normalized != *s.Code {
			return ErrEndpointResponse
		}
	}
	if !s.Node.valid() || s.Version < 1 || (role == "client" && !paired) {
		return ErrEndpointResponse
	}
	return nil
}

func (c *Client) sessionCall(ctx context.Context, call Call, id *SessionID) (Session, error) {
	s, err := endpoint[Session](ctx, c, call)
	if err != nil {
		return Session{}, err
	}
	if id != nil && s.SessionID != *id {
		return Session{}, ErrEndpointResponse
	}
	if err := s.validate(c.o.Role, call.Nonce); err != nil {
		return Session{}, err
	}
	return s, nil
}

// CreateSession requests code disclosure with a fresh signed nonce on every attempt.
func (c *Client) CreateSession(ctx context.Context, key string, node NodeID) (Session, error) {
	call := post("/v1/sessions", key, map[string]NodeID{"node_id": node})
	call.Nonce = true
	s, err := c.sessionCall(ctx, call, nil)
	if err == nil && s.Node.NodeID != node {
		return Session{}, ErrEndpointResponse
	}
	return s, err
}

func sessionPath(id SessionID) string { return "/v1/sessions/" + id.String() }

// Session reads immediately with since=0. Wait is expressed in whole seconds.
func (c *Client) Session(ctx context.Context, id SessionID, since int64, wait time.Duration, nonce bool) (Session, error) {
	if since < 0 || wait%time.Second != 0 {
		return Session{}, fmt.Errorf("%w: invalid poll parameters", ErrBadRequest)
	}
	call := get(sessionPath(id))
	call.Query = url.Values{}
	call.Nonce = nonce
	call.Wait = wait
	if since > 0 {
		call.Query.Set("since_version", strconv.FormatInt(since, 10))
	}
	if wait != 0 {
		call.Query.Set("wait_s", strconv.FormatInt(int64(wait/time.Second), 10))
	}
	return c.sessionCall(ctx, call, &id)
}

// Established commits the pairing after the data handshake.
func (c *Client) Established(ctx context.Context, key string, id SessionID, client identity.Fingerprint) (Session, error) {
	return c.sessionCall(ctx, post(sessionPath(id)+"/established", key, map[string]string{"client_fp": client.String()}), &id)
}

// End preserves the original reason when the session already ended.
func (c *Client) End(ctx context.Context, key string, id SessionID, reason string) (Session, error) {
	return c.sessionCall(ctx, post(sessionPath(id)+"/end", key, map[string]string{"reason": reason}), &id)
}

// Ticket checks routing metadata against a signed session read before release.
func (c *Client) Ticket(ctx context.Context, key string, id SessionID) (Ticket, error) {
	t, err := endpoint[Ticket](ctx, c, post(sessionPath(id)+"/tickets", key, struct{}{}))
	if err != nil {
		return Ticket{}, err
	}
	s, err := c.Session(ctx, id, 0, 0, false)
	if err != nil {
		return Ticket{}, err
	}
	peer, role := s.ClientFP, "wait"
	if c.o.Role == "client" {
		peer, role = &s.OperatorFP, "dial"
	}
	if !t.Node.valid() || peer == nil || t.Role != role || t.PeerFP != *peer || t.Node.NodeID != s.Node.NodeID || t.Node.HubFP != s.Node.HubFP || t.Node.Addr != s.Node.Addr || t.Node.Port != s.Node.Port {
		return Ticket{}, ErrEndpointResponse
	}
	return t, nil
}

// Redeem preserves leading zeros during code normalization.
func (c *Client) Redeem(ctx context.Context, key, code string) (Session, error) {
	code, err := centercode.NormalizeNumericCode(code)
	if err != nil {
		return Session{}, err
	}
	const path = "/v1/redeem"
	if c.o.Role != "client" {
		return Session{}, ErrBadRequest
	}
	body := struct {
		Code string `json:"code"`
		Salt string `json:"salt"`
	}{code, c.salt(path, key)}
	return c.sessionCall(ctx, post(path, key, body), nil)
}

// ClientSessions recovers live sessions after the redemption key expires.
func (c *Client) ClientSessions(ctx context.Context) ([]Session, error) {
	out, err := endpoint[struct {
		Sessions []Session `json:"sessions"`
	}](ctx, c, get("/v1/client/sessions"))
	if err != nil {
		return nil, err
	}
	for _, s := range out.Sessions {
		if s.State == "ended" {
			return nil, ErrEndpointResponse
		}
		if err := s.validate("client", false); err != nil {
			return nil, err
		}
	}
	return out.Sessions, nil
}

// Activate sends the public key corresponding to the configured signing key.
func (c *Client) Activate(ctx context.Context, key string, req Activation) (Activated, error) {
	if len(c.o.NodeKey) != ed25519.PrivateKeySize {
		return Activated{}, fmt.Errorf("%w: node key required", ErrBadRequest)
	}
	body := struct {
		Activation
		Pub string `json:"node_pub"`
	}{req, wire64.EncodeToString(c.o.NodeKey.Public().(ed25519.PublicKey))}
	out, err := endpoint[Activated](ctx, c, post("/v1/node/activate", key, body))
	if err == nil && (out.Gen == 0 || !out.NodePolicy.valid()) {
		return Activated{}, ErrEndpointResponse
	}
	return out, err
}

func (p NodePolicy) valid() bool {
	return (p.NodeState == "active" || p.NodeState == "disabled") && p.HeartbeatIntervalS > 0 && p.LeaseTTLS > 0
}

// Heartbeat sends one sequence number without an idempotency key.
func (c *Client) Heartbeat(ctx context.Context, req Heartbeat) (HeartbeatResult, error) {
	out, err := endpoint[HeartbeatResult](ctx, c, post(heartbeatPath, "", req))
	if err != nil {
		return HeartbeatResult{}, err
	}
	if !out.NodePolicy.valid() {
		return HeartbeatResult{}, ErrEndpointResponse
	}
	decisions := make(map[SessionID]bool, len(out.Renew)+len(out.Revoke))
	for _, r := range out.Renew {
		if decisions[r.SessionID] || r.LeaseS < 1 || r.LeaseS > out.LeaseTTLS {
			return HeartbeatResult{}, ErrEndpointResponse
		}
		decisions[r.SessionID] = true
	}
	for _, r := range out.Revoke {
		if decisions[r.SessionID] {
			return HeartbeatResult{}, ErrEndpointResponse
		}
		decisions[r.SessionID] = true
	}
	return out, nil
}
