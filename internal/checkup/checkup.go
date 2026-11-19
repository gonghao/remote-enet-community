// Package checkup observes local prerequisites without changing system networking.
package checkup

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/carside"
	"remote-enet/internal/netpriv"
	"remote-enet/internal/settings"
	"remote-enet/internal/transport"
	"remote-enet/internal/tunadapter"
)

// Status distinguishes skipped observations from successful probes.
type Status string

// Skipped observations must never imply that a prerequisite is available.
const (
	Pass    Status = "pass"
	Warning Status = "warning"
	Failed  Status = "failed"
	Skipped Status = "skipped"
)

// Finding contains only fixed categories, never addresses or raw error text.
type Finding struct {
	ID       string
	Index    int
	Status   Status
	Code     string
	Duration time.Duration
}

// Report describes one observation, not a guarantee of later availability.
type Report struct {
	StartedAt, FinishedAt        time.Time
	Complete, Canceled, TimedOut bool
	Findings                     []Finding
}

// Input is borrowed only for Run. PeerCode is a fixed locator result, and
// Endpoints are already pinned public endpoints from an invite or paired store.
type Input struct {
	Config     settings.Config
	Runtime    app.Snapshot
	Running    bool
	Endpoints  []transport.Endpoint
	PeerCode   string
	Permission netpriv.Status
}

// Direct-input results carry only syntax status across the checkup boundary.
const (
	DirectInputValid           = "direct_input_valid"
	DirectInputInvalidHost     = "direct_input_invalid_host"
	DirectInputInvalidPort     = "direct_input_invalid_port"
	DirectInputInvalidCode     = "direct_input_invalid_code"
	DirectInputAddressRequired = "direct_input_address_required"
)

type backend struct {
	interfaces func() ([]carside.NIC, error)
	lookup     func(context.Context, string) ([]netip.Addr, error)
	dial       func(context.Context, string) (io.Closer, error)
	port       func(context.Context, string, string) error
	routes     func(context.Context, []netip.Addr) (netip.Prefix, error)
}

func systemBackend() backend {
	return backend{
		interfaces: carside.Interfaces,
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dial: func(ctx context.Context, address string) (io.Closer, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", address)
		},
		port: probePort, routes: tunadapter.InspectAuto,
	}
}

// Run has a ten-second overall budget and never sends application bytes. Local
// port probes are disabled while running. Synchronous interface/filesystem APIs
// cannot be forcibly interrupted; cancellation is checked around each call.
func Run(ctx context.Context, in Input) Report { return run(ctx, in, systemBackend()) }

func run(ctx context.Context, in Input, b backend) Report {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r := Report{StartedAt: time.Now()}
	add := func(id string, index int, probe func() (Status, string)) {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		status, code := probe()
		r.Findings = append(r.Findings, Finding{ID: id, Index: index, Status: status, Code: code, Duration: time.Since(start)})
	}
	add("settings", 0, func() (Status, string) {
		if in.Config.Validate() != nil {
			return Failed, "settings_invalid"
		}
		return Pass, "settings_valid"
	})
	if status, ok := directInputStatus(in.PeerCode); in.Config.Role == settings.RoleClient && ok {
		add("peer_configuration", 0, func() (Status, string) { return status, in.PeerCode })
	} else if len(r.Findings) > 0 && r.Findings[0].Status != Failed {
		var nics []carside.NIC
		var interfaceErr error
		add("interfaces", 0, func() (Status, string) {
			nics, interfaceErr = b.interfaces()
			if interfaceErr != nil {
				return Failed, "interfaces_failed"
			}
			return Pass, "interfaces_available"
		})
		if in.Config.Role == settings.RoleClient {
			add("vehicle_adapter", 0, func() (Status, string) {
				if interfaceErr != nil {
					return Skipped, "interfaces_failed"
				}
				addr := netip.Addr{}
				if in.Config.Client.NICAddr != "" {
					addr, _ = netip.ParseAddr(in.Config.Client.NICAddr)
				}
				_, err := carside.SelectNICFrom(nics, in.Config.Client.NICName, addr)
				switch {
				case errors.Is(err, carside.ErrNICAmbiguous):
					return Failed, "adapter_ambiguous"
				case err != nil:
					return Failed, "adapter_unavailable"
				default:
					return Pass, "adapter_selected"
				}
			})
			add("peer_configuration", 0, func() (Status, string) {
				if len(in.Endpoints) == 0 {
					code := in.PeerCode
					if !peerCode(code) {
						code = "pairing_missing"
					}
					return Warning, code
				}
				return Pass, "peer_available"
			})
			for i, endpoint := range in.Endpoints[:min(len(in.Endpoints), 4)] {
				valid := true
				add("hub_dns", i+1, func() (Status, string) {
					if endpoint.Kind != "hub" || endpoint.Hub.IsZero() {
						valid = false
						return Failed, "endpoint_invalid"
					}
					status, code := resolve(ctx, b, endpoint.Addr)
					valid = status == Pass
					return status, code
				})
				add("hub_tcp", i+1, func() (Status, string) {
					if !valid {
						return Skipped, "endpoint_invalid"
					}
					probe, cancel := context.WithTimeout(ctx, 2*time.Second)
					defer cancel()
					conn, err := b.dial(probe, endpoint.Addr)
					if err != nil {
						return Failed, networkCode(err)
					}
					if err := conn.Close(); err != nil {
						return Failed, "socket_close_failed"
					}
					return Pass, "tcp_reachable"
				})
			}
			if len(in.Endpoints) > 4 {
				add("hub_candidates", 0, func() (Status, string) { return Warning, "endpoint_limit" })
			}
		} else {
			bind, _ := netip.ParseAddr(in.Config.Operator.Bind)
			present := false
			add("presentation_adapter", 0, func() (Status, string) {
				if interfaceErr != nil {
					return Skipped, "interfaces_failed"
				}
				for _, nic := range nics {
					if nic.Address == bind {
						present = true
						return Pass, "adapter_selected"
					}
				}
				return Failed, "adapter_unavailable"
			})
			for _, item := range []struct {
				id, network, address string
				requiresBind         bool
			}{
				{"hub_port", "tcp", in.Config.Operator.Listen, false},
				{"diagnostic_port", "tcp4", net.JoinHostPort(bind.String(), strconv.Itoa(in.Config.Operator.DiagnosticPort)), true},
				{"control_port", "tcp4", net.JoinHostPort(bind.String(), strconv.Itoa(in.Config.Operator.ControlPort)), true},
				{"discovery_port", "udp4", net.JoinHostPort(bind.String(), strconv.Itoa(in.Config.Operator.DiscoveryPort)), true},
			} {
				add(item.id, 0, func() (Status, string) {
					if in.Running {
						return Skipped, "runtime_active"
					}
					if item.requiresBind && !present {
						return Skipped, "adapter_unavailable"
					}
					probe, cancel := context.WithTimeout(ctx, time.Second)
					defer cancel()
					if err := b.port(probe, item.network, item.address); err != nil {
						return Failed, networkCode(err)
					}
					return Pass, "port_available"
				})
			}
			add("public_endpoint", 0, func() (Status, string) {
				if in.Config.Operator.Endpoint == "" {
					return Warning, "endpoint_missing"
				}
				status, code := resolve(ctx, b, in.Config.Operator.Endpoint)
				if status != Pass {
					return status, code
				}
				return Warning, "remote_check_required"
			})
			add("mgu_permission", 0, func() (Status, string) { return permission(in.Permission) })
			add("mgu_routes", 0, func() (Status, string) {
				if !in.Runtime.Direct.Address.IsValid() {
					return Skipped, "mgu_not_discovered"
				}
				if in.Runtime.Direct.Phase == app.DirectReady || in.Runtime.Direct.Phase == app.DirectStarting || in.Runtime.Direct.Phase == app.DirectStopping {
					return Skipped, "direct_active"
				}
				probe, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				_, err := b.routes(probe, []netip.Addr{in.Runtime.Direct.Address})
				switch {
				case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
					return Warning, networkCode(err)
				case errors.Is(err, tunadapter.ErrTargetConflict):
					return Failed, "target_conflict"
				case errors.Is(err, tunadapter.ErrConflict):
					return Failed, "local_conflict"
				case err != nil:
					return Warning, "routes_unavailable"
				default:
					return Pass, "routes_available"
				}
			})
		}
		add("peer_session", 0, func() (Status, string) {
			if !in.Running {
				return Skipped, "not_running"
			}
			if in.Runtime.Phase == app.PhaseConnected {
				return Pass, "session_connected"
			}
			return Warning, "waiting_for_peer"
		})
	}
	r.FinishedAt = time.Now()
	r.Canceled = errors.Is(ctx.Err(), context.Canceled)
	r.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	r.Complete = ctx.Err() == nil
	return r
}

func directInputStatus(code string) (Status, bool) {
	switch code {
	case DirectInputValid:
		return Pass, true
	case DirectInputInvalidHost, DirectInputInvalidPort, DirectInputInvalidCode:
		return Failed, true
	case DirectInputAddressRequired:
		return Warning, true
	default:
		return "", false
	}
}

func peerCode(code string) bool {
	switch code {
	case "pairing_missing", "pairing_invalid", "pairing_unavailable", "invite_invalid":
		return true
	}
	return false
}

func resolve(ctx context.Context, b backend, address string) (Status, string) {
	host, port, err := net.SplitHostPort(address)
	n, e := strconv.Atoi(port)
	if err != nil || e != nil || n < 1 || n > 65535 || host == "" {
		return Failed, "endpoint_invalid"
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.IsUnspecified() || ip.IsMulticast() || ip == netip.MustParseAddr("255.255.255.255") {
			return Failed, "endpoint_invalid"
		}
		return Pass, "literal_address"
	}
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ips, err := b.lookup(probe, host)
	if err != nil {
		return Failed, networkCode(err)
	}
	if len(ips) == 0 {
		return Failed, "dns_empty"
	}
	return Pass, "dns_resolved"
}

func permission(p netpriv.Status) (Status, string) {
	if p.Platform != runtime.GOOS {
		return Skipped, "permission_unobserved"
	}
	if p.Elevated {
		return Pass, "permission_available"
	}
	switch p.Platform {
	case "darwin":
		if p.HelperAvailable && p.AuthorizationRequired {
			return Pass, "authorization_on_demand"
		}
		return Warning, "helper_missing"
	case "windows":
		return Warning, "administrator_required"
	default:
		return Warning, "network_privilege_required"
	}
}

func networkCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "probe_timeout"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, syscall.EADDRINUSE):
		return "port_in_use"
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		return "address_unavailable"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "probe_timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns_failed"
	}
	return platformNetworkCode(err)
}

func probePort(ctx context.Context, network, address string) error {
	lc := net.ListenConfig{}
	if network == "udp4" {
		s, err := lc.ListenPacket(ctx, network, address)
		if err != nil {
			return err
		}
		return s.Close()
	}
	s, err := lc.Listen(ctx, network, address)
	if err != nil {
		return err
	}
	return s.Close()
}
