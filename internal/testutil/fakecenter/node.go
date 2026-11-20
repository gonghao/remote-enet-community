package fakecenter

import "slices"

// Usage counts cumulative bytes and paired seconds. A snapshot sums maxima
// from each generation, so retransmitting closed reports cannot double count.
type Usage struct {
	RX     uint64 `json:"rx_bytes"`
	TX     uint64 `json:"tx_bytes"`
	Paired uint64 `json:"paired_s"`
}
type report struct {
	Usage
	ID    string `json:"session_id"`
	State string `json:"state"`
}

func validateNode(a *call) string {
	version, kids := a.heartbeatInput.Version, a.heartbeatInput.Kids
	if a.op == "activate" {
		version, kids = a.activateInput.Version, a.activateInput.Kids
	}
	if !versionPattern.MatchString(version) || len(kids) > 16 {
		return "bad_request"
	}
	for _, kid := range kids {
		if !validBytes(kid, 8) {
			return "bad_request"
		}
	}
	if a.op == "activate" {
		return ""
	}
	load := a.heartbeatInput.Load
	if max(load.Conns, load.Sessions, load.Waiting, load.RX, load.TX) > 1<<53-1 {
		return "bad_request"
	}
	if a.heartbeatInput.Gen == 0 || a.heartbeatInput.Seq == 0 || len(a.heartbeatInput.Sessions) > 1000 || len(a.heartbeatInput.Closed) > 1000 {
		return "bad_request"
	}
	var waiting uint64
	seen := map[string]bool{}
	for i, list := range [][]report{a.heartbeatInput.Sessions, a.heartbeatInput.Closed} {
		for _, u := range list {
			if !validBytes(u.ID, 16) || seen[u.ID] || max(u.RX, u.TX, u.Paired) > 1<<53-1 || i == 0 && (u.State != "waiting" && u.State != "paired") {
				return "bad_request"
			}
			if i == 0 && u.State == "waiting" {
				waiting++
			}
			seen[u.ID] = true
		}
	}
	if load.Sessions != uint64(len(a.heartbeatInput.Sessions)) || load.Waiting != waiting {
		return "bad_request"
	}
	return ""
}
func (n *node) allocatable(now int64) bool {
	return n.gen != 0 && n.state == "active" && now < n.last+45
}
func (c *Center) full(n *node) bool {
	used := 0
	for _, s := range c.sessions {
		if s.node == n && !s.released {
			used++
		}
	}
	return used >= n.Capacity
}
func (c *Center) nodeView(n *node) object {
	state := "available"
	if c.full(n) {
		state = "full"
	}
	return object{"node_id": n.id, "hub_fp": n.FP.String(), "addr": n.Addr, "port": n.Port, "region": n.Region, "state": state, "load": n.load}
}
func (c *Center) nodeReply(a *call) object {
	state := "active"
	if a.n.state == "disabled" {
		state = "disabled"
	}
	return object{"node_state": state, "server_time": a.now + a.offset, "heartbeat_interval_s": 15, "lease_ttl_s": 300}
}
func (s *session) merge(gen uint32, u Usage) {
	prev := s.usage[gen]
	s.usage[gen] = Usage{max(prev.RX, u.RX), max(prev.TX, u.TX), max(prev.Paired, u.Paired)}
}
func (c *Center) nodeOperation(a *call) result {
	n := a.n
	if a.op == "activate" {
		n.gen++
		n.seq = 0
		n.pub = raw(a.activateInput.Pub)
		n.last = a.now
		n.kids = slices.Clone(a.activateInput.Kids)
		for _, s := range c.sessions {
			if s.node == n && !s.released {
				s.old = max(s.bound, s.old)
			}
		}
		return result{status: 200, gen: n.gen}
	}
	if a.heartbeatInput.Gen != n.gen {
		return failure("node_superseded")
	}
	if n.state == "disabled" {
		return failure("node_disabled")
	}
	if a.heartbeatInput.Seq <= n.seq {
		return failure("bad_request")
	}
	n.seq = a.heartbeatInput.Seq
	n.last = a.now
	n.kids = slices.Clone(a.heartbeatInput.Kids)
	n.load = int(min(a.heartbeatInput.Load.Sessions, uint64(n.Capacity)) * 100 / uint64(n.Capacity))
	revoked := map[string]string{}
	renew := []object{}
	for _, u := range a.heartbeatInput.Closed {
		s := c.sessions[u.ID]
		if s == nil || s.node != n || s.state != "ended" {
			continue
		}
		s.merge(n.gen, u.Usage)
		if a.now > s.old {
			s.released = true
		}
	}
	for _, u := range a.heartbeatInput.Sessions {
		s := c.sessions[u.ID]
		if s == nil || s.node != n || s.state == "open" {
			revoked[u.ID] = "admin_revoked"
			continue
		}
		s.merge(n.gen, u.Usage)
		if s.state == "ended" {
			revoked[s.id] = s.reason
			continue
		}
		if s.issued == 0 {
			revoked[s.id] = "admin_revoked"
			continue
		}
		deadline, reason := s.hard()
		lease := min(300, deadline-a.now-1)
		if lease < 1 {
			revoked[s.id] = reason
			continue
		}
		s.idle = a.now
		s.bound = max(s.bound, a.now+lease)
		renew = append(renew, object{"session_id": s.id, "lease_s": lease})
	}
	for _, s := range c.sessions {
		if s.node == n && s.state == "ended" && !s.released {
			revoked[s.id] = s.reason
		}
	}
	revoke := []object{}
	for _, id := range sorted(revoked) {
		revoke = append(revoke, object{"session_id": id, "reason": revoked[id]})
	}
	b := c.nodeReply(a)
	b["renew"] = renew
	b["revoke"] = revoke
	return ok(b)
}

// NodeStatus returns a copy of the last accepted generation, sequence and key IDs.
func (c *Center) NodeStatus(id string) (gen uint32, seq uint64, kids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.nodes[id]
	return n.gen, n.seq, slices.Clone(n.kids)
}
