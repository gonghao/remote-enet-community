package hubcenter

import (
	"bytes"
	"context"
	"slices"
	"time"

	"remote-enet/internal/center"
	"remote-enet/internal/hub"
)

const maxEntries = 1000
const maxCount = 1<<53 - 1

type trafficSample struct {
	at     time.Time
	rx, tx uint64
}

func (n *Node) sample() trafficSample {
	rx, tx := n.o.Server.TrafficTotals()
	return trafficSample{n.o.Now(), rx, tx}
}

func usage(u hub.Usage) center.ClosedUsage {
	return center.ClosedUsage{SessionID: center.SessionID(u.Session), RxBytes: min(u.RxBytes, maxCount), TxBytes: min(u.TxBytes, maxCount), PairedS: uint64(max(0, u.PairedFor/time.Second))}
}

func (n *Node) report(ctx context.Context, gen uint32, seq uint64, last, current trafficSample, closed []center.ClosedUsage) center.Heartbeat {
	live := n.o.Server.Usage()
	slices.SortFunc(live, func(a, b hub.Usage) int { return bytes.Compare(a.Session[:], b.Session[:]) })
	h := center.Heartbeat{Gen: gen, Seq: seq, Version: n.o.Version, Kids: n.kids(), Closed: closed[:min(len(closed), maxEntries)]}
	if len(live) > maxEntries {
		n.o.Logger.ErrorContext(ctx, "Heartbeat entry limit exceeded", "sessions", len(live), "closed", len(closed))
	}
	h.Load.Sessions = uint64(len(live))
	for i, u := range live {
		state := "paired"
		h.Load.Conns += uint64(u.Conns)
		if !u.Paired {
			state = "waiting"
			h.Load.Waiting++
		}
		if i < maxEntries {
			h.Sessions = append(h.Sessions, center.SessionUsage{ClosedUsage: usage(u), State: state})
		}
	}
	if elapsed := current.at.Sub(last.at).Seconds(); elapsed > 0 {
		h.Load.RxBps = uint64(min(float64(maxCount), float64(current.rx-last.rx)/elapsed))
		h.Load.TxBps = uint64(min(float64(maxCount), float64(current.tx-last.tx)/elapsed))
	}
	return h
}

func (n *Node) revoke(revocations []center.Revocation, previous []center.ClosedUsage) []center.ClosedUsage {
	// Revoke the full set before advertising any closed session. Rotate the
	// previously sent batch to the back so persistent revocations cannot starve
	// pending reports, even when the hub's seal cache has already expired.
	current := make(map[center.SessionID]center.ClosedUsage, len(revocations))
	for _, r := range revocations {
		current[r.SessionID] = usage(n.o.Server.Revoke(hub.SessionID(r.SessionID)))
	}
	closed := make([]center.ClosedUsage, 0, len(current))
	sent := min(len(previous), maxEntries)
	retain := func(batch []center.ClosedUsage) {
		for _, u := range batch {
			if _, ok := current[u.SessionID]; ok {
				closed = append(closed, u)
				delete(current, u.SessionID)
			}
		}
	}
	retain(previous[sent:])
	// Previously unsent sessions go first, then outstanding retries, then
	// new arrivals. A steady stream of arrivals cannot displace retries.
	retain(previous[:sent])
	added := len(closed)
	for _, u := range current {
		closed = append(closed, u)
	}
	slices.SortFunc(closed[added:], func(a, b center.ClosedUsage) int { return bytes.Compare(a.SessionID[:], b.SessionID[:]) })
	return closed
}
