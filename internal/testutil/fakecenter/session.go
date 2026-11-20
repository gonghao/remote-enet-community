package fakecenter

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"slices"
	"time"

	"remote-enet/internal/centercode"
	"remote-enet/internal/identity"
	"remote-enet/internal/ticket"
)

type session struct {
	id, state, reason, client, code                           string
	account                                                   *account
	operator                                                  *device
	node                                                      *node
	version, codeExp, redeemed, pair, idle, ended, bound, old int64
	released                                                  bool
	issued                                                    int
	budget                                                    map[string][]int64
	usage                                                     map[uint32]Usage
}

func (s *session) hard() (int64, string) {
	at := s.account.until
	switch s.state {
	case "open":
		if s.codeExp < at {
			return s.codeExp, "code_expired"
		}
	case "authorized":
		if s.pair < at {
			return s.pair, "pair_timeout"
		}
	case "active":
		at += 1800
	}
	return at, "account_expired"
}
func (c *Center) end(s *session, reason string, at int64) {
	if s.state == "ended" {
		return
	}
	delete(c.codes, s.code)
	s.code = ""
	s.state = "ended"
	s.reason = reason
	s.ended = at
	s.version++
	s.released = s.issued == 0 || at > s.bound
	c.notify()
}
func (c *Center) sweep(now int64) {
	for _, s := range c.sessions {
		if s.state != "ended" {
			at, reason := s.hard()
			if offline := s.node.last + 45; offline < at {
				at, reason = offline, "node_lost"
			}
			if idle := s.idle + 600; s.state == "active" && idle < at {
				at, reason = idle, "idle_timeout"
			}
			if now >= at {
				c.end(s, reason, at)
			} else if s.account.status == "disabled" {
				c.end(s, "account_disabled", now)
			} else if s.node.state == "disabled" {
				c.end(s, "node_disabled", now)
			}
		}
		if s.state == "ended" && now > s.bound {
			s.released = true
		}
	}
	for key, v := range c.records {
		if v.until <= now {
			delete(c.records, key)
		}
	}
	for k, until := range c.nonces {
		if now > until {
			delete(c.nonces, k)
		}
	}
}
func (c *Center) busy(d *device) bool {
	for _, s := range c.sessions {
		if s.operator == d && s.state != "ended" {
			return true
		}
	}
	return false
}
func (c *Center) deviceValid(d *device, now int64) bool {
	return d != nil && !d.revoked && (now < d.expires() || c.busy(d))
}
func (c *Center) occupancy(a *account) (used, live int, earliest int64) {
	for _, s := range c.sessions {
		if s.account != a || s.released {
			continue
		}
		used++
		if s.state != "ended" {
			live++
		} else if earliest == 0 || s.bound < earliest {
			earliest = s.bound
		}
	}
	return
}
func (c *Center) accountView(a *account, now int64) object {
	used, _, _ := c.occupancy(a)
	return object{"account_id": a.id, "label": a.label, "status": a.status, "valid_until": a.until, "quota": a.quota, "quota_in_use": used, "expiry_grace_s": 1800, "server_time": now}
}
func (c *Center) summaries(a *account) []object {
	out := []object{}
	for _, id := range sorted(c.sessions) {
		s := c.sessions[id]
		if s.account == a && s.state != "ended" {
			out = append(out, object{"session_id": id, "state": s.state, "node_id": s.node.id, "operator_fp": s.operator.fp})
		}
	}
	return out
}
func (c *Center) view(s *session, role string, nonce bool) object {
	b := object{"session_id": s.id, "state": s.state, "version": s.version, "operator_fp": s.operator.fp, "operator_label": s.operator.label, "node": c.nodeView(s.node)}
	if s.state == "ended" {
		b["end_reason"] = s.reason
		b["ended_at"] = s.ended
	} else if role == "operator" {
		b["expires_at"], _ = s.hard()
	}
	if s.client != "" {
		b["client_fp"] = s.client
		b["redeemed_at"] = s.redeemed
		b["pair_deadline"] = s.pair
	}
	if role == "operator" && nonce && s.state == "open" {
		b["code"] = s.code
		b["code_expires_at"] = s.codeExp
	}
	return b
}
func validate(a *call) string {
	switch a.op {
	case "login":
		v, e := centercode.NormalizeAccountCode(a.loginInput.AccountCode)
		if e != nil || len(a.loginInput.Label) > 64 || !validBytes(a.loginInput.Salt, 16) {
			return "bad_request"
		}
		a.loginInput.AccountCode = v
	case "redeem":
		v, e := centercode.NormalizeNumericCode(a.redeemInput.Code)
		if e != nil || !validBytes(a.redeemInput.Salt, 16) {
			return "bad_request"
		}
		a.redeemInput.Code = v
	case "create":
		if !validBytes(a.createInput.NodeID, 16) {
			return "bad_request"
		}
	case "established":
		fp, e := identity.ParseFingerprint(a.establishInput.ClientFP)
		if e != nil {
			return "bad_request"
		}
		a.establishInput.ClientFP = fp.String()
	case "end":
		switch a.endInput.Reason {
		case "rejected", "confirm_timeout", "pake_failed", "pairing_lost":
		default:
			if a.endInput.Reason != a.role+"_ended" {
				return "bad_request"
			}
		}
	case "activate", "heartbeat":
		return validateNode(a)
	}
	return ""
}
func accountError(a *account, now int64) result {
	if a.status == "disabled" {
		return failure("account_disabled")
	}
	if now >= a.until {
		return detail("account_expired", object{"valid_until": a.until})
	}
	return result{}
}
func (c *Center) execute(a *call) result {
	switch a.op {
	case "activate", "heartbeat":
		return c.nodeOperation(a)
	case "login":
		acct := c.accounts[a.loginInput.AccountCode]
		if acct == nil {
			return failure("account_code_invalid")
		}
		if res := accountError(acct, a.now); res.code != "" {
			return res
		}
		if a.d != nil && a.d.account != acct && c.busy(a.d) {
			return failure("device_busy")
		}
		count := 0
		for _, d := range c.devices {
			if d != a.d && d.account == acct && c.deviceValid(d, a.now) {
				count++
			}
		}
		if count >= 5 {
			return failure("device_limit")
		}
		if a.d == nil {
			a.d = &device{fp: a.fp}
			c.devices[a.fp] = a.d
		}
		a.d.account = acct
		a.d.label = a.loginInput.Label
		a.d.revoked = false
		a.d.used = a.now
		a.d.login = a.now
		return ok(nil)
	case "account":
		return ok(object{"account": c.accountView(a.d.account, a.now), "expires_at": a.d.expires(), "sessions": c.summaries(a.d.account)})
	case "devices":
		list := []object{}
		for _, fp := range sorted(c.devices) {
			d := c.devices[fp]
			if d.account == a.d.account && !d.revoked {
				list = append(list, object{"device_fp": fp, "label": d.label, "current": d == a.d, "last_login_at": d.login, "last_used_at": d.used})
			}
		}
		return ok(object{"devices": list})
	case "revoke":
		d := c.devices[a.id]
		if d == nil || d.account != a.d.account {
			return failure("not_found")
		}
		d.revoked = true
		for _, s := range c.sessions {
			if s.operator == d {
				c.end(s, "device_revoked", a.now)
			}
		}
		return ok(object{})
	case "nodes":
		list := []object{}
		for _, id := range sorted(c.nodes) {
			n := c.nodes[id]
			if n.allocatable(a.now) {
				list = append(list, c.nodeView(n))
			}
		}
		return ok(object{"nodes": list})
	case "create":
		return c.create(a)
	case "redeem":
		s := c.codes[a.redeemInput.Code]
		if s == nil {
			return failure("code_invalid")
		}
		delete(c.codes, s.code)
		s.code = ""
		s.state = "authorized"
		s.client = a.fp
		s.redeemed = a.now
		s.pair = a.now + 600
		s.version++
		return result{status: 200, sid: s.id}
	case "clientSessions":
		list := []object{}
		for _, id := range sorted(c.sessions) {
			s := c.sessions[id]
			if s.client == a.fp && s.state != "ended" {
				list = append(list, c.view(s, "client", false))
			}
		}
		return ok(object{"sessions": list})
	case "end":
		c.end(a.s, a.endInput.Reason, a.now)
		return result{status: 200, sid: a.s.id}
	case "tickets":
		return c.issue(a)
	case "established":
		s := a.s
		if a.role != "operator" {
			return failure("forbidden")
		}
		if s.state == "ended" {
			return detail("session_ended", object{"end_reason": s.reason})
		}
		if s.client != a.establishInput.ClientFP || s.state == "open" {
			return detail("state_conflict", object{"state": s.state})
		}
		if s.state != "active" {
			s.state = "active"
			s.idle = a.now
			s.version++
		}
		return result{status: 200, sid: s.id}
	}
	panic("unhandled operation")
}
func (c *Center) create(a *call) result {
	acct := a.d.account
	if res := accountError(acct, a.now); res.code != "" {
		return res
	}
	if c.busy(a.d) {
		return failure("device_busy")
	}
	used, live, bound := c.occupancy(acct)
	if used >= acct.quota {
		if live < acct.quota {
			return waitError("quota_releasing", min(15, bound-a.now+1))
		}
		return detail("quota_exhausted", object{"sessions": c.summaries(acct)})
	}
	n := c.nodes[a.createInput.NodeID]
	if n == nil || !n.allocatable(a.now) || c.full(n) {
		return failure("node_unavailable")
	}
	if len(c.codes) >= 1000 {
		return waitError("rate_limited", 1)
	}
	var code string
	for {
		v, err := rand.Int(rand.Reader, big.NewInt(100000000))
		if err != nil {
			panic(err)
		}
		code = fmt.Sprintf("%08d", v.Int64())
		if c.codes[code] == nil {
			break
		}
	}
	s := &session{id: randomID(), state: "open", version: 1, code: code, codeExp: a.now + 600, account: acct, node: n, operator: a.d, budget: map[string][]int64{}, usage: map[uint32]Usage{}}
	c.sessions[s.id] = s
	c.codes[code] = s
	return result{status: 201, sid: s.id}
}
func (c *Center) issue(a *call) result {
	s := a.s
	if s.state == "open" {
		return waitError("session_not_ready", 1)
	}
	if s.state == "ended" {
		return detail("session_ended", object{"end_reason": s.reason})
	}
	deadline, reason := s.hard()
	exp := min(deadline-60, a.now+120)
	if exp <= a.now {
		return detail("session_ended", object{"end_reason": reason})
	}
	recent := slices.DeleteFunc(s.budget[a.role], func(t int64) bool { return t <= a.now-60 })
	s.budget[a.role] = recent
	if len(recent) >= c.opts.TicketBudget {
		return waitError("rate_limited", recent[0]+60-a.now)
	}
	role, name, peer := uint8(2), "wait", s.client
	if a.role == "client" {
		role, name, peer = 1, "dial", s.operator.fp
	}
	dev, _ := identity.ParseFingerprint(a.fp)
	other, _ := identity.ParseFingerprint(peer)
	now := time.Unix(a.now, 0)
	claims := ticket.CenterClaims{Center: c.opts.CenterID, Account: [16]byte(raw(s.account.id)), Session: [16]byte(raw(s.id)), Node: [16]byte(raw(s.node.id)), JTI: [16]byte(raw(randomID())), Device: dev, Peer: other, Hub: s.node.FP, Role: role, IssuedAt: now, NotBefore: now, Generation: s.node.gen}
	b, err := ticket.SignCenter(c.key, claims, ticket.CenterIssuePolicy{TicketTTL: 120, TicketMaxTTL: 300, ClockSkew: 30, HardDeadline: time.Unix(deadline, 0)})
	if err != nil {
		return failure("internal")
	}
	s.issued++
	s.bound = max(s.bound, exp+60)
	s.budget[a.role] = append(recent, a.now)
	return ok(object{"ticket": encoding.EncodeToString(b), "ticket_expires_at": exp, "role": name, "peer_fp": peer, "node": c.nodeView(s.node)})
}
