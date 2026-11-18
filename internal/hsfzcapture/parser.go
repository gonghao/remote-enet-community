package hsfzcapture

import (
	"encoding/binary"
	"encoding/hex"
	"time"
)

const maxBody = 16 << 20

type parser struct {
	expected uint64
	start    uint64
	at       time.Time
	header   [6]byte
	prefix   [256]byte
	count    uint64
	length   uint32
	control  uint16
	disabled bool
}

func (p *parser) emit(r *Recorder, s *recording, c *Connection, d Direction, status string) {
	f := Frame{Connection: c.id, Generation: c.meta.Generation, Offset: p.start, At: p.at, Network: c.meta.Network, Channel: c.meta.Channel, Direction: d, Control: p.control, BodyLength: p.length, Status: status, PrefixHex: hex.EncodeToString(p.prefix[:min(p.count, 256)]), PrefixTruncated: p.count > 256}
	if (p.control == 1 || p.control == 2) && p.count >= 8 {
		f.HasAddresses = true
		f.Source = p.prefix[6]
		f.Destination = p.prefix[7]
	}
	r.mu.Lock()
	r.snap.Frames++
	f.Sequence = r.snap.Frames
	if status != "complete" {
		r.snap.Incomplete = true
	}
	if len(r.snap.Recent) == r.opt.RecentFrames {
		copy(r.snap.Recent, r.snap.Recent[1:])
		r.snap.Recent[len(r.snap.Recent)-1] = f
	} else {
		r.snap.Recent = append(r.snap.Recent, f)
	}
	r.mu.Unlock()
	r.write(s, event{Type: "frame", At: f.At, conn: c, Direction: d, Offset: f.Offset, Frame: &f}, false)
}
func (p *parser) gap(r *Recorder, s *recording, c *Connection, d Direction) {
	if p.disabled {
		return
	}
	if p.at.IsZero() {
		p.at = time.Now()
	}
	p.emit(r, s, c, d, "gap")
	p.disabled = true
}
func (p *parser) feed(r *Recorder, s *recording, c *Connection, d Direction, offset uint64, at time.Time, b []byte) {
	if p.disabled {
		return
	}
	if offset != p.expected {
		p.gap(r, s, c, d)
		return
	}
	p.expected += uint64(len(b))
	for _, v := range b {
		if p.count == 0 {
			p.start = offset
			p.at = at
			p.length = 0
			p.control = 0
		}
		offset++
		if p.count < 256 {
			p.prefix[p.count] = v
		}
		if p.count < 6 {
			p.header[p.count] = v
		}
		p.count++
		if p.count == 6 {
			p.length = binary.BigEndian.Uint32(p.header[:4])
			p.control = binary.BigEndian.Uint16(p.header[4:])
			if p.length > maxBody {
				p.emit(r, s, c, d, "oversized")
				p.disabled = true
				return
			}
		}
		if p.count >= 6 && p.count == uint64(p.length)+6 {
			p.emit(r, s, c, d, "complete")
			p.count = 0
		}
	}
}
func (p *parser) finish(r *Recorder, s *recording, c *Connection, d Direction, observed uint64) {
	if p.disabled {
		return
	}
	if observed != p.expected {
		p.gap(r, s, c, d)
	} else if p.count > 0 {
		p.emit(r, s, c, d, "partial")
		p.count = 0
	}
}
