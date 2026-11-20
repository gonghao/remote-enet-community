package fakecenter

import (
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"remote-enet/internal/center"
	"remote-enet/internal/identity"
)

// Match numeric prerelease identifiers separately to exclude leading zeroes.
const number = `(0|[1-9][0-9]*)`
const pre = `(` + number + `|[0-9]*[A-Za-z-][0-9A-Za-z-]*)`

var versionPattern = regexp.MustCompile(`^` + number + `\.` + number + `\.` + number + `(-` + pre + `(\.` + pre + `)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

type loginInput struct {
	AccountCode string `json:"account_code"`
	Label       string `json:"device_label"`
	Salt        string `json:"salt"`
}
type redeemInput struct {
	Code string `json:"code"`
	Salt string `json:"salt"`
}
type createInput struct {
	NodeID string `json:"node_id"`
}
type establishInput struct {
	ClientFP string `json:"client_fp"`
}
type endInput struct {
	Reason string `json:"reason"`
}
type activateInput struct {
	Pub     string   `json:"node_pub"`
	Version string   `json:"version"`
	Kids    []string `json:"kids"`
}
type heartbeatInput struct {
	Version string   `json:"version"`
	Kids    []string `json:"kids"`
	Gen     uint32   `json:"gen"`
	Seq     uint64   `json:"seq"`
	Load    struct {
		Conns    uint64 `json:"conns"`
		Sessions uint64 `json:"sessions"`
		Waiting  uint64 `json:"waiting"`
		RX       uint64 `json:"rx_bps"`
		TX       uint64 `json:"tx_bps"`
	} `json:"load"`
	Sessions []report `json:"sessions"`
	Closed   []report `json:"closed"`
}
type call struct {
	loginInput                    loginInput
	redeemInput                   redeemInput
	createInput                   createInput
	establishInput                establishInput
	endInput                      endInput
	activateInput                 activateInput
	heartbeatInput                heartbeatInput
	role, fp, path, op, id, nonce string
	n                             *node
	d                             *device
	s                             *session
	now, ts, offset               int64
}
type result struct {
	status int
	code   string
	body   object
	sid    string
	gen    uint32
}

func ok(b object) result { return result{status: 200, body: b} }

var statuses = map[string]int{"unavailable": 503, "internal": 500, "rate_limited": 429, "quota_releasing": 409, "session_not_ready": 409, "clock_skew": 401, "nonce_reused": 409, "bad_request": 400, "idempotency_key_reused": 422, "upgrade_required": 426, "unauthenticated": 401, "account_code_invalid": 401, "account_disabled": 403, "account_expired": 403, "device_limit": 403, "device_busy": 409, "quota_exhausted": 409, "node_unavailable": 409, "code_invalid": 404, "state_conflict": 409, "session_ended": 410, "not_found": 404, "forbidden": 403, "node_unknown": 404, "node_key_mismatch": 403, "node_check_failed": 409, "node_superseded": 409, "node_disabled": 403}

func failure(code string) result {
	return result{status: statuses[code], code: code, body: object{"message": strings.ReplaceAll(code, "_", " "), "code": code}}
}
func detail(code string, details object) result {
	r := failure(code)
	r.body["details"] = details
	return r
}
func waitError(code string, seconds int64) result {
	r := failure(code)
	r.body["retry_after_s"] = max(1, seconds)
	return r
}

type scope struct{ principal, role, path, key string }
type record struct {
	digest  [32]byte
	result  result
	until   int64
	account string
}

func (c *Center) serve(entry int, w http.ResponseWriter, r *http.Request) {
	target, err := center.CanonicalTarget(r.RequestURI)
	path, _, _ := strings.Cut(target, "?")
	if err != nil {
		path = ""
	}
	k := faultKey{entry, path}
	c.mu.Lock()
	c.calls[k]++
	var f Fault
	if q := c.faults[k]; len(q) > 0 {
		f = q[0]
		c.faults[k] = q[1:]
	}
	c.mu.Unlock()
	if f.Delay > 0 {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(f.Delay):
		}
	}
	// Keep transport-level retries on reused connections from hiding dropped responses.
	w.Header().Set("Connection", "close")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Renet-Request-Id", randomID())
	w.Header().Set("Content-Type", "application/json")
	res, replayed := c.handle(r, path, f)
	if f.DropResponse {
		panic(http.ErrAbortHandler)
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.WriteHeader(res.status)
	if res.code != "" {
		res.body = object{"error": res.body}
	}
	if res.body != nil {
		_ = json.MarshalWrite(w, res.body)
	}
}
func (c *Center) handle(r *http.Request, path string, f Fault) (result, bool) {
	limit := int64(16384)
	if path == "/v1/node/heartbeat" {
		c.mu.Lock()
		n := c.nodes[r.Header.Get("Renet-Node-Id")]
		known := n != nil && n.gen != 0
		c.mu.Unlock()
		if !known {
			return failure("unauthenticated"), false
		}
		limit = 512 * 1024
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		return failure("bad_request"), false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	a := call{now: c.now(), path: path, nonce: r.Header.Get("Renet-Nonce"), offset: int64(f.TimeOffset / time.Second)}
	c.sweep(a.now)
	if path == "/v1/meta" && r.Method == "GET" {
		role, version, _ := strings.Cut(r.Header.Get("Renet-Client"), "/")
		if len(r.Header.Values("Renet-Client")) != 1 || (role != "hub" && role != "client" && role != "operator") || !versionPattern.MatchString(version) {
			return failure("bad_request"), false
		}
		if f.Code != "" {
			return failure(f.Code), false
		}
		return ok(object{"center_id": c.opts.CenterID, "api_versions": []string{"v1"}, "min_app_version": object{"operator": "0.0.0-0", "client": "0.0.0-0", "hub": "0.0.0-0"}, "server_time": a.now + a.offset}), false
	}
	if path == "/v1/node/activate" && json.Unmarshal(body, &a.activateInput) != nil {
		return failure("bad_request"), false
	}
	a.role, _, _ = strings.Cut(r.Header.Get("Renet-Client"), "/")
	req := center.Request{CenterID: c.opts.CenterID, Target: r.RequestURI, Method: r.Method, Body: body}
	principal := r.Header.Get("Renet-Node-Id")
	hub := strings.HasPrefix(path, "/v1/node/")
	if hub {
		if !validBytes(principal, 16) {
			return failure("bad_request"), false
		}
		a.n = c.nodes[principal]
		pub := raw(a.activateInput.Pub)
		if path != "/v1/node/activate" {
			if a.n == nil {
				return failure("unauthenticated"), false
			}
			pub = a.n.pub
		} else if len(pub) != 32 {
			return failure("bad_request"), false
		}
		err = center.VerifyNode(req, r.Header, [16]byte(raw(principal)), pub)
		if err == nil {
			if a.n == nil {
				return failure("node_unknown"), false
			}
			if identity.FingerprintOf(pub) != a.n.FP {
				return failure("node_key_mismatch"), false
			}
		}
	} else {
		err = center.VerifyDevice(req, r.Header)
		principal = r.Header.Get("Renet-Device-Key")
		a.fp = identity.FingerprintOf(raw(principal)).String()
		a.d = c.devices[a.fp]
	}
	if err != nil {
		if errors.Is(err, center.ErrBadRequest) {
			return failure("bad_request"), false
		}
		return failure("unauthenticated"), false
	}
	a.ts, _ = strconv.ParseInt(r.Header.Get("Renet-Timestamp"), 10, 64)
	if center.CheckTimestamp(a.now, a.ts, center.RequestWindow) != nil {
		return detail("clock_skew", object{"server_time": a.now + a.offset}), false
	}
	if code := c.route(&a, r.Method); code != "" {
		return failure(code), false
	}
	if a.role == "operator" && a.op != "login" {
		if !c.deviceValid(a.d, a.now) {
			return failure("unauthenticated"), false
		}
		a.d.used = a.now
	}
	var dest any
	switch a.op {
	case "login":
		dest = &a.loginInput
	case "redeem":
		dest = &a.redeemInput
	case "create":
		dest = &a.createInput
	case "end":
		dest = &a.endInput
	case "established":
		dest = &a.establishInput
	case "heartbeat":
		dest = &a.heartbeatInput
	default:
		dest = &struct{}{}
	}
	if len(body) > 0 && (json.Unmarshal(body, dest) != nil || strings.TrimSpace(string(body)) == "null") {
		return failure("bad_request"), false
	}
	if code := validate(&a); code != "" {
		return failure(code), false
	}
	key := scope{principal, a.role, path, r.Header.Get("Idempotency-Key")}
	digest := sha256.Sum256(body)
	writing := r.Method == "POST" && a.op != "heartbeat"
	rec, hit := c.records[key]
	hit = writing && hit && rec.until > a.now
	if hit && rec.digest != digest {
		return failure("idempotency_key_reused"), false
	}
	if a.nonce != "" && (a.op == "create" || a.op == "session" && a.role == "operator" && a.s.state == "open") {
		if until, found := c.nonces[a.nonce]; found && a.now <= until {
			return failure("nonce_reused"), false
		}
		if a.op == "session" || hit && rec.result.code == "" {
			c.nonces[a.nonce] = a.ts + 60
		}
	}
	if hit {
		if rec.account != "" && (a.d == nil || a.d.revoked || a.d.account.id != rec.account) {
			return failure("unauthenticated"), false
		}
		return c.render(&a, rec.result), true
	}
	var res result
	if f.Code != "" {
		res = failure(f.Code)
		if f.RetryAfter > 0 {
			res.body["retry_after_s"] = f.RetryAfter
		}
		if f.Code == "clock_skew" {
			res.body["details"] = object{"server_time": a.now + a.offset}
		}
	} else if a.op == "session" {
		res = c.poll(r, &a)
	} else {
		res = c.withBudget(&a)
	}
	if a.op == "create" && res.code == "" && a.nonce != "" {
		c.nonces[a.nonce] = a.ts + 60
	}
	if writing && cacheable(res) {
		rec = record{digest: digest, result: res, until: a.now + 86400}
		if a.role == "operator" && a.d != nil {
			rec.account = a.d.account.id
		}
		c.records[key] = rec
	}
	if r.Method == "POST" && res.code == "" {
		c.notify()
	}
	return c.render(&a, res), false
}
func cacheable(r result) bool {
	switch r.code {
	case "bad_request", "unauthenticated", "rate_limited", "quota_releasing", "session_not_ready", "clock_skew", "nonce_reused", "unavailable", "internal":
		return false
	}
	return true
}
func (c *Center) render(a *call, r result) result {
	if r.code != "" {
		return r
	}
	if r.sid != "" {
		r.body = c.view(c.sessions[r.sid], a.role, a.op == "create" && a.nonce != "")
	}
	if r.gen != 0 {
		if a.n.gen != r.gen {
			return failure("node_superseded")
		}
		r.body = c.nodeReply(a)
		r.body["gen"] = r.gen
	}
	if a.op == "login" {
		r.body = object{"account": c.accountView(a.d.account, a.now), "device": object{"label": a.d.label, "device_fp": a.fp, "expires_at": a.d.expires()}}
	}
	return r
}
func (c *Center) route(a *call, method string) string {
	route := method + " " + a.path
	role := "operator"
	switch route {
	case "POST /v1/operator/login":
		a.op = "login"
	case "GET /v1/operator/account":
		a.op = "account"
	case "GET /v1/operator/devices":
		a.op = "devices"
	case "GET /v1/nodes":
		a.op = "nodes"
	case "POST /v1/sessions":
		a.op = "create"
	case "POST /v1/redeem":
		a.op = "redeem"
		role = "client"
	case "GET /v1/client/sessions":
		a.op = "clientSessions"
		role = "client"
	case "POST /v1/node/activate":
		a.op = "activate"
		role = "hub"
	case "POST /v1/node/heartbeat":
		a.op = "heartbeat"
		role = "hub"
	default:
		parts := strings.Split(a.path, "/")
		if len(parts) == 6 && parts[1] == "v1" && parts[2] == "operator" && parts[3] == "devices" && parts[5] == "revoke" && method == "POST" {
			fp, err := identity.ParseFingerprint(parts[4])
			if err != nil {
				return "bad_request"
			}
			a.id = fp.String()
			a.op = "revoke"
			break
		}
		if len(parts) < 4 || parts[1] != "v1" || parts[2] != "sessions" {
			return "not_found"
		}
		a.s = c.sessions[parts[3]]
		if a.s == nil {
			return "not_found"
		}
		op := a.fp == a.s.operator.fp
		cl := a.fp == a.s.client && (a.s.state != "ended" || a.now < a.s.ended+600)
		if !op && !cl {
			return "not_found"
		}
		if a.role == "operator" && op {
			if a.s.account != a.s.operator.account {
				return "not_found"
			}
		} else if a.role == "client" && cl {
			role = "client"
		} else {
			return "bad_request"
		}
		if len(parts) == 4 && method == "GET" {
			a.op = "session"
		} else if len(parts) == 5 && method == "POST" && (parts[4] == "tickets" || parts[4] == "established" || parts[4] == "end") {
			a.op = parts[4]
		} else {
			return "not_found"
		}
	}
	if role != a.role {
		return "bad_request"
	}
	return ""
}
func (c *Center) poll(r *http.Request, a *call) result {
	q := r.URL.Query()
	parse := func(k string) (int64, bool) {
		if !q.Has(k) {
			return 0, true
		}
		v, e := strconv.ParseInt(q.Get(k), 10, 64)
		return v, e == nil && v >= 0
	}
	since, ok1 := parse("since_version")
	wait, ok2 := parse("wait_s")
	if !ok1 || !ok2 || wait > 25 {
		return failure("bad_request")
	}
	stop := time.Now().Add(time.Duration(wait) * time.Second)
	for q.Has("since_version") && a.s.version <= since && time.Now().Before(stop) {
		ch := c.changed
		c.mu.Unlock()
		// Also observe lazy time transitions when there are no writes.
		timer := time.NewTimer(min(time.Second, time.Until(stop)))
		select {
		case <-r.Context().Done():
			timer.Stop()
			c.mu.Lock()
			return failure("unavailable")
		case <-ch:
		case <-timer.C:
		}
		timer.Stop()
		c.mu.Lock()
		a.now = c.now()
		c.sweep(a.now)
		if code := c.route(a, "GET"); code != "" {
			return failure(code)
		}
		if a.role == "operator" && !c.deviceValid(a.d, a.now) {
			return failure("unauthenticated")
		}
	}
	return ok(c.view(a.s, a.role, a.nonce != ""))
}
