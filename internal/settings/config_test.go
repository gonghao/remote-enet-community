package settings

import (
	"runtime"
	"strings"
	"testing"

	"remote-enet/internal/identity"
)

func TestDefault(t *testing.T) {
	want := Config{
		Version: 2, Role: "client", AllowLinkTest: true,
		Operator: Operator{
			Mode:   "direct",
			Listen: "0.0.0.0:47000", DiagnosticPort: 6801, ControlPort: 6811,
			DiscoveryPort: 6811, Exclusive: runtime.GOOS == "windows",
		},
	}
	if got := Default(); !got.Equal(want) {
		t.Fatalf("default = %+v, want %+v", got, want)
	}
	for _, role := range []string{RoleClient, RoleOperator} {
		cfg := Default()
		cfg.Role = role
		if err := cfg.Validate(); err != nil {
			t.Fatalf("unconfigured %s defaults: %v", role, err)
		}
	}
}

func TestValidateRoleAndVersion(t *testing.T) {
	for _, version := range []int{-1, 0, 1, 3} {
		cfg := Default()
		cfg.Version = version
		if err := cfg.Validate(); err == nil {
			t.Errorf("accepted version %d", version)
		}
	}
	for _, role := range []string{"", "Client", "OPERATOR", "hub", "client "} {
		cfg := Default()
		cfg.Role = role
		if err := cfg.Validate(); err == nil {
			t.Errorf("accepted role %q", role)
		}
	}
	cfg := Default()
	cfg.Operator.Listen = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty inactive listener: %v", err)
	}
	cfg.Role = RoleOperator
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted empty active listener")
	}
}

func TestValidateLiteralIPv4(t *testing.T) {
	valid := []string{"", "127.0.0.1", "169.254.12.34", "192.168.1.20", "10.0.0.1"}
	invalid := []string{
		"localhost", "192.168.1.20/24", "0.0.0.0", "255.255.255.255",
		"224.0.0.1", "239.255.255.250", "::", "::1", "fe80::1%eth0",
		"::ffff:192.168.1.20", "192.168.1.999", "192.168.1.20:6801",
		" 192.168.1.20", "192.168.1.20 ", "192.168.001.020", " ",
	}
	for _, field := range []string{"client.nic_addr", "operator.bind"} {
		for _, role := range []string{RoleClient, RoleOperator} {
			t.Run(field+"/"+role, func(t *testing.T) {
				for _, values := range []struct {
					items []string
					valid bool
				}{{valid, true}, {invalid, false}} {
					for _, value := range values.items {
						cfg := Default()
						cfg.Role = role
						if field == "client.nic_addr" {
							cfg.Client.NICAddr = value
						} else {
							cfg.Operator.Bind = value
						}
						err := cfg.Validate()
						if (err == nil) != values.valid {
							t.Errorf("%q: valid=%t, error=%v", value, values.valid, err)
						}
						if err != nil && !strings.Contains(err.Error(), field) {
							t.Errorf("error lacks field context: %v", err)
						}
					}
				}
			})
		}
	}
}

func TestValidateClientPreferences(t *testing.T) {
	fp := identity.Fingerprint{1, 2, 3}
	for _, role := range []string{RoleClient, RoleOperator} {
		for _, peer := range []string{"", fp.String(), strings.ToLower(fp.String()), strings.ReplaceAll(fp.String(), "-", "")} {
			cfg := Default()
			cfg.Role, cfg.Client.Peer, cfg.Client.NICName = role, peer, "Ethernet adapter 车侧"
			if err := cfg.Validate(); err != nil {
				t.Errorf("valid client preferences in %s role: %v", role, err)
			}
		}
		for _, peer := range []string{identity.Fingerprint{}.String(), fp.Short(), "not-a-fingerprint", "renet1:secret", " " + fp.String(), fp.String() + "\n"} {
			cfg := Default()
			cfg.Role, cfg.Client.Peer = role, peer
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "client.peer") {
				t.Errorf("peer %q in %s role: %v", peer, role, err)
			} else if strings.Contains(err.Error(), peer) {
				t.Errorf("invalid peer contents leaked in error: %v", err)
			}
		}
	}
	for _, name := range []string{"Ethernet\x00", "Ethernet\n", "\t", "Ethernet\x7f", "\xff"} {
		cfg := Default()
		cfg.Client.NICName = name
		if err := cfg.Validate(); err == nil {
			t.Errorf("accepted invalid NIC name %q", name)
		}
	}
}

func TestValidateAddresses(t *testing.T) {
	valid := []string{
		"localhost:47000", "home.example.com:47000", "HOME.Example.:1",
		"my-home:65535", "xn--bcher-kva.example:47000", "123.example:47000",
		"127.0.0.1:47000", "192.168.1.10:47000", "169.254.12.34:47000",
		"[::1]:47000", "[2001:db8::1]:47000", "[fe80::1%eth0]:47000",
		"[::ffff:127.0.0.1]:47000",
	}
	invalid := []string{
		"home.example", "home.example:", "home.example:0", "home.example:65536",
		"home.example:-1", "home.example:+1", "home.example:abc", "home.example:http",
		"home.example:1.5", "home.example:47000/path", "home.example: 47000",
		" home.example:47000", "home.example :47000", "home.example:47000 ",
		"https://home.example:47000", "user@home.example:47000", "bad/host:47000",
		"bad\\host:47000", "bad_host:47000", "bad..host:47000", ".home:47000",
		"-home.example:47000", "home-.example:47000", "host\n:47000", "b\x00ad:47000",
		strings.Repeat("a", 64) + ".example:47000", strings.Repeat("a.", 127) + "a:47000",
		"999.1.1.1:47000", "127.0.0:47000", "127.000.000.001:47000", "12345:47000",
		"[home.example]:47000", "[127.0.0.1]:47000", "[2001:db8::zzz]:47000",
		"::1:47000", "224.0.0.1:47000", "255.255.255.255:47000", "[ff02::1]:47000",
		"[::ffff:224.0.0.1]:47000", "[::ffff:255.255.255.255]:47000",
	}
	wildcards := []string{":47000", "0.0.0.0:47000", "[::]:47000", "[::ffff:0.0.0.0]:47000"}
	for _, role := range []string{RoleClient, RoleOperator} {
		for _, listen := range []bool{false, true} {
			label := "operator.endpoint"
			if listen {
				label = "operator.listen"
			}
			t.Run(role+"/"+label, func(t *testing.T) {
				for _, cases := range []struct {
					values []string
					valid  bool
				}{{valid, true}, {invalid, false}, {wildcards, listen}} {
					for _, value := range cases.values {
						cfg := Default()
						cfg.Role = role
						if listen {
							cfg.Operator.Listen = value
						} else {
							cfg.Operator.Endpoint = value
						}
						err := cfg.Validate()
						if (err == nil) != cases.valid {
							t.Errorf("%q: valid=%t, error=%v", value, cases.valid, err)
						}
						if err != nil && !strings.Contains(err.Error(), label) {
							t.Errorf("error lacks field context: %v", err)
						}
					}
				}
			})
		}
	}
}

func TestValidatePorts(t *testing.T) {
	for _, field := range []string{"diagnostic_port", "control_port", "discovery_port"} {
		for _, value := range []int{-1, 0, 65536, int(^uint(0) >> 1)} {
			cfg := Default()
			switch field {
			case "diagnostic_port":
				cfg.Operator.DiagnosticPort = value
			case "control_port":
				cfg.Operator.ControlPort = value
			case "discovery_port":
				cfg.Operator.DiscoveryPort = value
			}
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), field) {
				t.Errorf("%s=%d: %v", field, value, err)
			}
		}
	}
	cfg := Default()
	cfg.Operator.ControlPort = cfg.Operator.DiagnosticPort
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted equal diagnostic and control TCP ports")
	}
	for _, discovery := range []int{1, 65535} {
		cfg := Default()
		cfg.Operator.DiagnosticPort, cfg.Operator.ControlPort = 1, 65535
		cfg.Operator.DiscoveryPort = discovery
		cfg.Operator.Exclusive = true
		if err := cfg.Validate(); err != nil {
			t.Errorf("TCP/UDP port reuse at %d: %v", discovery, err)
		}
	}
}
