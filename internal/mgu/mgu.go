// Package mgu performs the read-only MGU IPv4 configuration query.
package mgu

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"time"

	"remote-enet/internal/hsfz"
	"remote-enet/internal/proto"
	"remote-enet/internal/transport"
)

// Opener must honor cancellation during tunnel establishment.
type Opener interface {
	OpenTunnel(context.Context, proto.OpenTunnel) (transport.Stream, error)
}

// Info preserves the final two wire values without assigning routing semantics.
type Info struct {
	Address netip.Addr
	Netmask netip.Addr
	Third   netip.Addr
}

// Options bounds the entire operation, including tunnel establishment.
type Options struct {
	Tester    byte          // Zero defaults to F4.
	Timeout   time.Duration // Zero defaults to 5s; maximum 30s.
	MaxFrames int           // Zero defaults to 64; maximum 1024.
}

// Discover owns and closes only its tunnel. The opener's session stays usable.
func Discover(ctx context.Context, opener Opener, gateway netip.AddrPort, opts Options) (info Info, err error) {
	if !validIPv4(gateway.Addr(), true) || gateway.Port() == 0 {
		return Info{}, fmt.Errorf("mgu: invalid gateway %s", gateway)
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.MaxFrames == 0 {
		opts.MaxFrames = 64
	}
	if opts.Tester == 0 {
		opts.Tester = 0xF4
	}
	if opts.Timeout < 0 || opts.Timeout > 30*time.Second || opts.MaxFrames < 1 || opts.MaxFrames > 1024 {
		return Info{}, fmt.Errorf("mgu: invalid operation limits")
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Info{}, fmt.Errorf("mgu: %w", err)
	}
	st, err := opener.OpenTunnel(ctx, proto.OpenTunnel{Kind: "tcp", Target: gateway.String()})
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return Info{}, fmt.Errorf("mgu: open tunnel: %w", err)
	}
	// Join the cancellation callback before returning; no query worker outlives us.
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(closed); _ = st.Close() })
	defer func() {
		if !stop() {
			<-closed
		}
		_ = st.Close()
		if ctx.Err() != nil {
			info = Info{}
			err = fmt.Errorf("mgu: %w", ctx.Err())
		}
	}()
	req := hsfz.Frame{Control: hsfz.ControlDiag, Body: hsfz.DiagBody{Src: opts.Tester, Dst: 0x63, Payload: []byte{0x22, 0x17, 0x2A}}.Marshal()}
	if err := hsfz.WriteFrame(st, req); err != nil {
		return Info{}, fmt.Errorf("mgu: request: %w", err)
	}
	for range opts.MaxFrames {
		f, err := hsfz.ReadFrame(st, 4096)
		if err != nil {
			return Info{}, fmt.Errorf("mgu: response: %w", err)
		}
		switch f.Control {
		case hsfz.ControlAliveCheck:
			if err := hsfz.WriteFrame(st, hsfz.AliveResponse(opts.Tester)); err != nil {
				return Info{}, fmt.Errorf("mgu: alive response: %w", err)
			}
		case hsfz.ControlAck:
			continue
		case hsfz.ControlDiag:
			d, err := hsfz.ParseDiagBody(f.Body)
			if err != nil {
				return Info{}, fmt.Errorf("mgu: diagnostic frame: %w", err)
			}
			if d.Src != 0x63 || d.Dst != opts.Tester {
				continue
			}
			p := d.Payload
			if len(p) == 0 {
				return Info{}, fmt.Errorf("mgu: empty diagnostic response")
			}
			if p[0] == 0x7F {
				if len(p) != 3 {
					return Info{}, fmt.Errorf("mgu: malformed negative response")
				}
				if p[1] != 0x22 {
					continue
				}
				if p[2] == 0x78 {
					continue
				}
				return Info{}, fmt.Errorf("mgu: ReadDataByIdentifier rejected: NRC 0x%02X", p[2])
			}
			if p[0] != 0x62 {
				continue
			}
			if len(p) < 3 {
				return Info{}, fmt.Errorf("mgu: truncated positive response")
			}
			if !bytes.Equal(p[:3], []byte{0x62, 0x17, 0x2A}) {
				continue
			}
			if len(p) < 16 || p[3] != 0 {
				return Info{}, fmt.Errorf("mgu: malformed IP configuration response")
			}
			address := netip.AddrFrom4([4]byte(p[4:8]))
			if !validIPv4(address, false) {
				return Info{}, fmt.Errorf("mgu: invalid MGU address %s", address)
			}
			return Info{Address: address, Netmask: netip.AddrFrom4([4]byte(p[8:12])), Third: netip.AddrFrom4([4]byte(p[12:16]))}, nil
		case hsfz.ControlErrIncorrectTesterAddress, hsfz.ControlErrIncorrectControlWord, hsfz.ControlErrIncorrectFormat, hsfz.ControlErrIncorrectDestAddress, hsfz.ControlErrMessageTooLarge, hsfz.ControlErrAppNotReady, hsfz.ControlErrOutOfMemory:
			return Info{}, fmt.Errorf("mgu: HSFZ error control 0x%04X", f.Control)
		default:
			return Info{}, fmt.Errorf("mgu: unexpected HSFZ control 0x%04X", f.Control)
		}
	}
	return Info{}, fmt.Errorf("mgu: response frame limit exceeded (%d)", opts.MaxFrames)
}

func validIPv4(a netip.Addr, loopback bool) bool {
	if !a.Is4() || a.IsUnspecified() || a.IsMulticast() || (!loopback && a.IsLoopback()) {
		return false
	}
	b := a.As4()
	return b[0] != 0 && b[0] < 224 && a != netip.MustParseAddr("255.255.255.255")
}
