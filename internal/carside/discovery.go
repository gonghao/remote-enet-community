package carside

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"syscall"
	"time"

	"remote-enet/internal/hsfz"
	"remote-enet/internal/proto"
)

type DiscoverOptions struct {
	NIC          NIC
	Destinations []netip.AddrPort
	Timeout      time.Duration
	Logger       *slog.Logger
}

// IdentRaw retains the complete UDP datagram, including its six-byte envelope.
type Gateway struct {
	Address        netip.Addr
	Identification hsfz.Identification
	IdentRaw       []byte
}

// Discover uses one source-bound socket; timeouts return all valid replies collected.
func Discover(ctx context.Context, o DiscoverOptions) ([]Gateway, error) {
	if err := validateNIC(o.NIC); err != nil {
		return nil, err
	}
	if o.Timeout < 0 {
		return nil, fmt.Errorf("carside: discovery timeout must not be negative")
	}
	if o.Timeout == 0 {
		o.Timeout = 3 * time.Second
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	destinations := o.Destinations
	if len(destinations) == 0 {
		destinations = []netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("255.255.255.255"), 6811)}
		if o.NIC.Prefix.Bits() < 31 {
			destinations = append(destinations, netip.AddrPortFrom(directedBroadcast(o.NIC.Prefix), 6811))
		}
	}
	for _, target := range destinations {
		if !target.IsValid() || !target.Addr().Is4() || target.Port() == 0 {
			return nil, fmt.Errorf("carside: invalid IPv4 discovery destination %s", target)
		}
	}
	listen := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		return bindVehicleInterface(ctx, o.NIC, raw)
	}}
	packet, err := listen.ListenPacket(ctx, "udp4", netip.AddrPortFrom(o.NIC.Address, 0).String())
	if err != nil {
		return nil, fmt.Errorf("carside: bind discovery source %s: %w", o.NIC.Address, err)
	}
	conn := packet.(*net.UDPConn)
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline := time.Now().Add(o.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("carside: set discovery deadline: %w", err)
	}
	if err := sendDiscovery(ctx, o, destinations, conn.WriteToUDPAddrPort); err != nil {
		return nil, err
	}
	gateways := make(map[netip.Addr]Gateway)
	buffer := make([]byte, 65535)
	for {
		n, from, err := conn.ReadFromUDPAddrPort(buffer)
		if err != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if parentDeadline, ok := ctx.Deadline(); ok && !time.Now().Before(parentDeadline) {
				return nil, context.DeadlineExceeded
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				break
			}
			return nil, fmt.Errorf("carside: receive discovery: %w", err)
		}
		if !from.Addr().Is4() || !o.NIC.Prefix.Contains(from.Addr()) || from.Addr().IsUnspecified() || from.Addr().IsMulticast() || from.Addr() == o.NIC.Address || broadcast(from.Addr(), o.NIC.Prefix) || network(from.Addr(), o.NIC.Prefix) {
			continue
		}
		raw := buffer[:n]
		if len(raw) < 6 || uint64(binary.BigEndian.Uint32(raw)) != uint64(len(raw)-6) || hsfz.Control(binary.BigEndian.Uint16(raw[4:])) != hsfz.ControlVehicleIdent {
			continue
		}
		id, err := hsfz.ParseIdentification(raw[6:])
		if err != nil {
			continue
		}
		gateways[from.Addr()] = Gateway{Address: from.Addr(), Identification: id, IdentRaw: bytes.Clone(raw)}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]Gateway, 0, len(gateways))
	for _, gateway := range gateways {
		result = append(result, gateway)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Address.Compare(result[j].Address) < 0 })
	return result, nil
}

func sendDiscovery(ctx context.Context, o DiscoverOptions, destinations []netip.AddrPort, write func([]byte, netip.AddrPort) (int, error)) error {
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var sent bool
	var sendErr error
	seen := make(map[netip.AddrPort]struct{}, len(destinations))
	for _, target := range destinations {
		if _, duplicate := seen[target]; duplicate {
			continue
		}
		seen[target] = struct{}{}
		if _, err := write(hsfz.IdentRequest, target); err != nil {
			logger.WarnContext(ctx, "discovery send failed", "destination", target, "interface", o.NIC.Name, "source", o.NIC.Address, "error", err)
			sendErr = fmt.Errorf("carside: send discovery to %s: %w", target, err)
		} else {
			sent = true
		}
	}
	if !sent {
		if err := ctx.Err(); err != nil {
			return err
		}
		return sendErr
	}
	return nil
}

// VehicleState copies the full identification datagram independently of the gateway cache.
func (g Gateway) VehicleState(nic NIC, seq uint64) proto.VehicleState {
	return proto.VehicleState{
		NIC: nic.Name, NICAddr: nic.Address.String(), GatewayAddr: g.Address.String(),
		VIN: g.Identification.VIN, MAC: net.HardwareAddr(g.Identification.MAC[:]).String(),
		DiagAddr: g.Identification.DiagAddr, IdentRaw: bytes.Clone(g.IdentRaw), Seq: seq,
	}
}
