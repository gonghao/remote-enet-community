package carside

import (
	"errors"
	"net/netip"
	"testing"
)

func testNIC() NIC {
	return NIC{Name: "test-loopback", Address: netip.MustParseAddr("127.0.0.1"), Prefix: netip.MustParsePrefix("127.0.0.0/8")}
}

func TestSelectNICIsExplicitOrUniqueLinkLocal(t *testing.T) {
	lo := testNIC()
	lan := NIC{Name: "lan", Address: netip.MustParseAddr("192.168.1.2"), Prefix: netip.MustParsePrefix("192.168.1.0/24")}
	link := NIC{Name: "vehicle", Address: netip.MustParseAddr("169.254.1.2"), Prefix: netip.MustParsePrefix("169.254.0.0/16")}
	other := link
	other.Address = netip.MustParseAddr("169.254.2.3")
	for _, tc := range []struct {
		name string
		nics []NIC
		nic  string
		addr netip.Addr
		want NIC
		ok   bool
	}{
		{name: "unique", nics: []NIC{lo, lan, link}, want: link, ok: true},
		{name: "no-auto-loopback", nics: []NIC{lo}},
		{name: "no-auto-lan", nics: []NIC{lan}},
		{name: "ambiguous-link-local", nics: []NIC{link, other}},
		{name: "ambiguous-named-addresses", nics: []NIC{link, other}, nic: "vehicle"},
		{name: "explicit-loopback", nics: []NIC{lo, lan}, nic: lo.Name, want: lo, ok: true},
		{name: "explicit-lan", nics: []NIC{lo, lan}, addr: lan.Address, want: lan, ok: true},
		{name: "explicit-address", nics: []NIC{link, other}, nic: "vehicle", addr: other.Address, want: other, ok: true},
		{name: "conflicting-selectors", nics: []NIC{lo, lan}, nic: lo.Name, addr: lan.Address},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectNICFrom(tc.nics, tc.nic, tc.addr)
			if (err == nil) != tc.ok || tc.ok && got != tc.want {
				t.Fatalf("selection = %+v, %v", got, err)
			}
		})
	}
}

func TestInterfacesCanSelectExplicitLoopback(t *testing.T) {
	nics, err := Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, nic := range nics {
		if err := validateNIC(nic); err != nil {
			t.Fatal(err)
		}
		if nic.Address.IsLoopback() {
			found = true
			selected, err := SelectNIC(nic.Name, nic.Address)
			if err != nil || selected != nic {
				t.Fatalf("select loopback = %+v, %v", selected, err)
			}
		}
	}
	if !found {
		t.Fatal("no up IPv4 loopback interface")
	}
	if _, err := SelectNIC("", netip.MustParseAddr("::1")); err == nil {
		t.Fatal("IPv6 selector accepted")
	}
}

func TestValidateNICAndBroadcastBoundaries(t *testing.T) {
	for _, tc := range []struct {
		address string
		prefix  string
		ok      bool
	}{
		{"169.254.1.1", "169.254.0.0/16", true},
		{"127.0.0.1", "127.0.0.0/8", true},
		{"192.168.1.1", "192.168.1.0/24", true},
		{"192.168.1.0", "192.168.1.0/31", true},
		{"192.168.1.1", "192.168.1.1/32", true},
		{"192.168.1.0", "192.168.1.0/24", false},
		{"192.168.1.255", "192.168.1.0/24", false},
		{"0.0.0.0", "0.0.0.0/0", false},
		{"224.0.0.1", "224.0.0.0/24", false},
		{"255.255.255.255", "0.0.0.0/0", false},
		{"192.168.1.1", "192.168.2.0/24", false},
		{"::1", "::/0", false},
	} {
		t.Run(tc.address+"/"+tc.prefix, func(t *testing.T) {
			err := validateNIC(NIC{Address: netip.MustParseAddr(tc.address), Prefix: netip.MustParsePrefix(tc.prefix)})
			if (err == nil) != tc.ok {
				t.Fatalf("validation = %v", err)
			}
		})
	}
	if got := directedBroadcast(netip.MustParsePrefix("169.254.13.14/16")); got.String() != "169.254.255.255" {
		t.Fatal(got)
	}
}

func TestSelectionDistinguishesMissingAndAmbiguous(t *testing.T) {
	nic := testNIC()
	if _, err := SelectNICFrom(nil, nic.Name, nic.Address); !errors.Is(err, ErrNICUnavailable) {
		t.Fatalf("missing source: %v", err)
	}
	other := nic
	other.Address = netip.MustParseAddr("127.0.0.3")
	if _, err := SelectNICFrom([]NIC{nic, other}, nic.Name, netip.Addr{}); !errors.Is(err, ErrNICAmbiguous) {
		t.Fatalf("ambiguous source: %v", err)
	}
	if _, err := SelectNICFrom([]NIC{other}, nic.Name, nic.Address); !errors.Is(err, ErrNICUnavailable) {
		t.Fatalf("fixed source drifted: %v", err)
	}
	for _, addr := range []string{"::1", "0.0.0.0", "224.0.0.1", "255.255.255.255"} {
		if _, err := SelectNICFrom(nil, "", netip.MustParseAddr(addr)); err == nil || errors.Is(err, ErrNICUnavailable) {
			t.Fatalf("invalid selector %s: %v", addr, err)
		}
	}
}
