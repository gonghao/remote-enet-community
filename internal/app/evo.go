package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"time"

	"remote-enet/internal/l3"
	"remote-enet/internal/proto"
)

var evoAddresses = [...]netip.Addr{
	netip.AddrFrom4([4]byte{169, 254, 199, 119}),
	netip.AddrFrom4([4]byte{169, 254, 199, 99}),
}

func discoverEVO(ctx context.Context, opener l3.Opener) (netip.Addr, error) {
	var failures error
	for _, address := range evoAddresses {
		if err := ctx.Err(); err != nil {
			return netip.Addr{}, err
		}
		target := netip.AddrPortFrom(address, 22)
		err := probeEVO(ctx, opener, target)
		if err == nil {
			return address, nil
		}
		failures = errors.Join(failures, fmt.Errorf("%s: %w", target, err))
	}
	if err := ctx.Err(); err != nil {
		return netip.Addr{}, err
	}
	return netip.Addr{}, fmt.Errorf("evo: no SSH endpoint responded: %w", failures)
}

func probeEVO(ctx context.Context, opener l3.Opener, target netip.AddrPort) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	stream, err := opener.OpenTunnel(ctx, proto.OpenTunnel{Kind: "tcp", Target: target.String()})
	if err != nil {
		return err
	}
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(closed)
		_ = stream.Close()
	})
	defer func() {
		if stop() {
			_ = stream.Close()
		} else {
			<-closed
		}
	}()

	reader := bufio.NewReader(io.LimitReader(stream, 4096))
	for {
		line, err := reader.ReadSlice('\n')
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return fmt.Errorf("read SSH identification: %w", err)
		}
		if !bytes.HasPrefix(line, []byte("SSH-")) {
			continue
		}
		if len(line) > 255 {
			return fmt.Errorf("invalid SSH identification line")
		}
		identification := bytes.TrimSuffix(line[4:len(line)-1], []byte("\r"))
		version, software, _ := bytes.Cut(identification, []byte("-"))
		name, _, _ := bytes.Cut(software, []byte(" "))
		if len(name) == 0 || !bytes.Equal(version, []byte("2.0")) && !bytes.Equal(version, []byte("1.99")) {
			return fmt.Errorf("invalid SSH version or software name")
		}
		for _, b := range software {
			if b < 32 || b > 126 {
				return fmt.Errorf("invalid SSH identification character")
			}
		}
		return nil
	}
}
