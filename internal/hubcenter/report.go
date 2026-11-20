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
	if len(live) > maxEntries || len(closed) > maxEntries {
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
	closed := make([]center.ClosedUsage, 0, len(revocations))
	for _, r := range revocations {
		final := usage(n.o.Server.Revoke(hub.SessionID(r.SessionID)))
		// A prolonged outage can outlast the server seal cache. Keep the
		// original final totals until the center acknowledges this session.
		i, found := slices.BinarySearchFunc(previous, r.SessionID, func(u center.ClosedUsage, id center.SessionID) int { return bytes.Compare(u.SessionID[:], id[:]) })
		if found {
			final = previous[i]
		}
		closed = append(closed, final)
	}
	slices.SortFunc(closed, func(a, b center.ClosedUsage) int { return bytes.Compare(a.SessionID[:], b.SessionID[:]) })
	return closed
}
