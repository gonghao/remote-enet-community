// Command fakezgw runs explicit transport fixtures for isolated tool testing.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"

	"remote-enet/internal/hsfz"
	"remote-enet/internal/testutil/fakezgw"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		slog.Error("fake ZGW failed", "error", err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("fakezgw", flag.ContinueOnError)
	fs.SetOutput(stderr)
	bind := fs.String("bind", "127.0.0.1", "local bind IP")
	vin := fs.String("vin", "WBAXXXXXXX0000001", "17-byte test VIN")
	mac := fs.String("mac", "AA:BB:CC:DD:EE:FF", "test MAC")
	diagAddr := fs.Uint("diag-addr", 0x10, "diagnostic address (decimal or 0x hex)")
	tester := fs.Uint("tester", 0xf4, "expected alive tester address")
	alive := fs.Duration("alive", 0, "test alive interval; 0 disables checks (no measured vehicle default)")
	timeout := fs.Duration("alive-timeout", 0, "test alive response timeout; 0 disables disconnect")
	aliveResponse := fs.Uint("alive-response-control", uint(hsfz.ControlStatusInquiry), "expected alive response control: 0x13, or explicit experimental 0x12")
	short := fs.String("short-alive-body", "", "explicit two-byte hex body for short alive checks")
	kl15 := fs.String("kl15", "", "explicit simulated KL15 byte in hex; unset leaves queries unanswered")
	name := fs.String("profile", "normal", "normal|no-alive|drop-alive|long-alive|error-frame")
	maxConns := fs.Int("max-conns", 0, "maximum simultaneous TCP connections; 0 is unlimited")
	responses := fs.String("responders", "", "JSON file mapping hex UDS requests to hex UDS response strings")
	multiResponses := fs.String("multi-responders", "", "JSON file mapping hex requests to explicit source/payload response lists")
	nrc := fs.String("unknown-nrc", "", "explicit hex NRC for unknown requests; unset means silence after ACK")
	diagPort := fs.Int("diag-port", 6801, "diagnostic TCP port")
	ctrlPort := fs.Int("ctrl-port", 6811, "control TCP port")
	discPort := fs.Int("disc-port", 6811, "discovery UDP port")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	ip, err := netip.ParseAddr(*bind)
	if err != nil {
		return fmt.Errorf("parse bind: %w", err)
	}
	m, err := net.ParseMAC(*mac)
	if err != nil || len(m) != 6 {
		return fmt.Errorf("MAC must contain six bytes")
	}
	if *diagAddr > 255 || *tester == 0 || *tester > 255 {
		return fmt.Errorf("diagnostic and tester addresses must fit in a byte, tester must be nonzero")
	}
	if *aliveResponse != uint(hsfz.ControlStatusInquiry) && *aliveResponse != uint(hsfz.ControlAliveCheck) {
		return fmt.Errorf("alive-response-control must be 0x13 or experimental 0x12")
	}
	p := fakezgw.Profile{Ident: hsfz.Identification{DiagAddr: byte(*diagAddr), VIN: *vin}, TesterAddr: byte(*tester), AliveInterval: *alive, AliveTimeout: *timeout, AliveResponseControl: hsfz.Control(*aliveResponse), MaxConns: *maxConns}
	copy(p.Ident.MAC[:], m)
	if *short != "" {
		b, err := hex.DecodeString(*short)
		if err != nil || len(b) != 2 {
			return fmt.Errorf("short-alive-body must be two hex bytes")
		}
		p.ShortAliveBody = &[2]byte{b[0], b[1]}
	}
	parseByte := func(s string) (byte, error) {
		b, err := hex.DecodeString(s)
		if err != nil || len(b) != 1 {
			return 0, fmt.Errorf("expected one hex byte, got %q", s)
		}
		return b[0], nil
	}
	if *kl15 != "" {
		b, err := parseByte(*kl15)
		if err != nil {
			return err
		}
		p.KL15 = &b
	}
	if *nrc != "" {
		b, err := parseByte(*nrc)
		if err != nil {
			return err
		}
		p.Unknown = fakezgw.NegativeResponse(b)
	}
	switch *name {
	case "normal":
	case "no-alive":
		p.AliveInterval = 0
	case "drop-alive":
		if p.AliveInterval <= 0 || p.AliveTimeout <= 0 {
			return fmt.Errorf("drop-alive requires explicit positive --alive and --alive-timeout")
		}
	case "long-alive":
		p.AliveLongBody = true
		if p.AliveInterval <= 0 {
			return fmt.Errorf("long-alive requires explicit positive --alive")
		}
	case "error-frame":
		p.Faults = []fakezgw.Fault{fakezgw.FaultIncorrectControl}
	default:
		return fmt.Errorf("unknown profile %q", *name)
	}
	if *responses != "" {
		b, err := os.ReadFile(*responses)
		if err != nil {
			return fmt.Errorf("read responders: %w", err)
		}
		var encoded map[string]string
		if err := json.Unmarshal(b, &encoded); err != nil {
			return fmt.Errorf("parse responders: %w", err)
		}
		p.Responders = make(map[string][]byte, len(encoded))
		for k, v := range encoded {
			payload, err := hex.DecodeString(v)
			if err != nil {
				return fmt.Errorf("decode responder %s: %w", k, err)
			}
			p.Responders[k] = payload
		}
	}
	if *multiResponses != "" {
		b, err := os.ReadFile(*multiResponses)
		if err != nil {
			return fmt.Errorf("read multi responders: %w", err)
		}
		p.MultiResponders, err = decodeMultiResponders(b)
		if err != nil {
			return err
		}
	}
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	s, err := fakezgw.Start(ctx, ip, p, fakezgw.WithPorts(*diagPort, *ctrlPort, *discPort), fakezgw.WithLogger(logger))
	if err != nil {
		return err
	}
	defer s.Close()
	diag, ctrl, disc := s.Addr()
	logger.Info("fake ZGW listening", "diag", diag.String(), "control", ctrl.String(), "discovery", disc.String(), "profile", *name, "alive_interval", p.AliveInterval, "alive_timeout", p.AliveTimeout, "alive_response_control", fmt.Sprintf("0x%04x", p.AliveResponseControl), "timings", "configured test values")
	<-ctx.Done()
	s.Close()
	logger.Info("fake ZGW stopped", "stats", s.Stats())
	return nil
}

func decodeMultiResponders(raw []byte) (map[string][]hsfz.DiagBody, error) {
	type response struct {
		Source  *uint8  `json:"source"`
		Payload *string `json:"payload"`
	}
	var encoded map[string][]response
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&encoded); err != nil {
		return nil, fmt.Errorf("parse multi responders: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("multi responders must contain one JSON object")
	}
	result := make(map[string][]hsfz.DiagBody, len(encoded))
	for request, responses := range encoded {
		key, err := hex.DecodeString(request)
		if err != nil || len(key) == 0 {
			return nil, fmt.Errorf("multi responder request must be nonempty hex: %q", request)
		}
		normalized := hex.EncodeToString(key)
		if _, exists := result[normalized]; exists {
			return nil, fmt.Errorf("duplicate normalized multi responder request %s", normalized)
		}
		result[normalized] = make([]hsfz.DiagBody, 0, len(responses))
		for _, r := range responses {
			if r.Source == nil || r.Payload == nil {
				return nil, fmt.Errorf("multi responder %s requires explicit source and payload", request)
			}
			payload, err := hex.DecodeString(*r.Payload)
			if err != nil {
				return nil, fmt.Errorf("decode multi responder %s: %w", request, err)
			}
			result[normalized] = append(result[normalized], hsfz.DiagBody{Src: *r.Source, Payload: payload})
		}
	}
	return result, nil
}
