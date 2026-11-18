package tunadapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/tun"
)

type fakeNative struct {
	done         chan struct{}
	events       chan tun.Event
	input        chan []byte
	output       chan []byte
	once         sync.Once
	log          *[]string
	blockWrite   bool
	writeStarted chan struct{}
}

func newFake() *fakeNative {
	return &fakeNative{done: make(chan struct{}), events: make(chan tun.Event), input: make(chan []byte, 4), output: make(chan []byte, 4), writeStarted: make(chan struct{}, 1)}
}
func (f *fakeNative) File() *os.File           { return nil }
func (f *fakeNative) Name() (string, error)    { return "enettest", nil }
func (f *fakeNative) MTU() (int, error)        { return 1500, nil }
func (f *fakeNative) BatchSize() int           { return 1 }
func (f *fakeNative) Events() <-chan tun.Event { return f.events }
func (f *fakeNative) Close() error {
	f.once.Do(func() {
		if f.log != nil {
			*f.log = append(*f.log, "native close")
		}
		for range 8 {
			f.events <- tun.EventUp
		}
		close(f.done)
		close(f.events)
	})
	return nil
}
func (f *fakeNative) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case <-f.done:
		return 0, io.EOF
	case p := <-f.input:
		sizes[0] = copy(bufs[0][offset:], p)
		return 1, nil
	}
}
func (f *fakeNative) Write(bufs [][]byte, offset int) (int, error) {
	select {
	case f.writeStarted <- struct{}{}:
	default:
	}
	if f.blockWrite {
		<-f.done
		return 0, io.ErrClosedPipe
	}
	p := append([]byte(nil), bufs[0][offset:]...)
	select {
	case <-f.done:
		return 0, io.ErrClosedPipe
	case f.output <- p:
		return 1, nil
	}
}
func testConfig() Config {
	return Config{Name: "enettest", Local: netip.MustParsePrefix("192.0.2.1/32"), Targets: []netip.Addr{netip.MustParseAddr("198.51.100.2")}}
}
func testBackend(f *fakeNative) backend {
	return backend{os: "linux", interfaces: func() ([]net.Interface, error) { return nil, nil }, addresses: func(net.Interface) ([]net.Addr, error) { return nil, nil }, routes: func(context.Context) ([]routeEntry, error) { return nil, nil }, create: func(string, int) (tun.Device, error) { return f, nil }, run: func(context.Context, []string) error { return nil }}
}
func TestValidationBeforeCreation(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"local prefix", func(c *Config) { c.Local = netip.MustParsePrefix("192.0.2.1/24") }},
		{"loopback", func(c *Config) { c.Targets = []netip.Addr{netip.MustParseAddr("127.0.0.1")} }},
		{"duplicate", func(c *Config) { c.Targets = append(c.Targets, c.Targets[0]) }},
		{"local target", func(c *Config) { c.Targets = []netip.Addr{c.Local.Addr()} }},
		{"mtu", func(c *Config) { c.MTU = 65536 }},
		{"name", func(c *Config) { c.Name = "x;reboot" }},
		{"budget", func(c *Config) { c.Targets = make([]netip.Addr, 257) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testConfig()
			tt.edit(&c)
			b := testBackend(newFake())
			b.create = func(string, int) (tun.Device, error) { t.Fatal("created invalid device"); return nil, nil }
			if _, err := open(context.Background(), c, b); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
}
func TestPreflightConflicts(t *testing.T) {
	for _, kind := range []string{"name", "address", "route"} {
		t.Run(kind, func(t *testing.T) {
			b := testBackend(newFake())
			b.create = func(string, int) (tun.Device, error) { t.Fatal("created conflicting device"); return nil, nil }
			switch kind {
			case "name":
				b.interfaces = func() ([]net.Interface, error) { return []net.Interface{{Name: "enettest"}}, nil }
			case "address":
				b.interfaces = func() ([]net.Interface, error) { return []net.Interface{{Name: "management"}}, nil }
				b.addresses = func(net.Interface) ([]net.Addr, error) {
					return []net.Addr{&net.IPNet{IP: net.ParseIP("198.51.100.1"), Mask: net.CIDRMask(24, 32)}}, nil
				}
			case "route":
				b.routes = func(context.Context) ([]routeEntry, error) {
					return []routeEntry{{Prefix: netip.MustParsePrefix("198.51.100.0/24")}}, nil
				}
			}
			if _, err := open(context.Background(), testConfig(), b); err == nil {
				t.Fatal("accepted conflict")
			}
		})
	}
}
func TestRollbackEveryStep(t *testing.T) {
	c := testConfig()
	c.MTU = 1500
	c.Targets = append(c.Targets, netip.MustParseAddr("203.0.113.2"))
	for _, platform := range []string{"linux", "darwin", "windows"} {
		if platform == "darwin" {
			c.Name = "utun"
		} else {
			c.Name = "enettest"
		}
		ops := plan(platform, "enettest", c)
		for fail := range ops {
			t.Run(platform+string(rune('0'+fail)), func(t *testing.T) {
				f := newFake()
				var log []string
				f.log = &log
				b := testBackend(f)
				b.os = platform
				count := 0
				boom := errors.New("configuration failure")
				b.run = func(_ context.Context, args []string) error {
					log = append(log, strings.Join(args, " "))
					count++
					if count == fail+1 {
						return boom
					}
					return nil
				}
				_, err := open(context.Background(), c, b)
				if !errors.Is(err, boom) {
					t.Fatalf("failure lost: %v", err)
				}
				var want []string
				for i := 0; i <= fail; i++ {
					want = append(want, strings.Join(ops[i].add, " "))
				}
				for i := fail - 1; i >= 0; i-- {
					if len(ops[i].remove) > 0 {
						want = append(want, strings.Join(ops[i].remove, " "))
					}
				}
				want = append(want, "native close")
				if !reflect.DeepEqual(log, want) {
					t.Fatalf("cleanup order\ngot %v\nwant %v", log, want)
				}
			})
		}
	}
}
func TestPacketCancellationAndClose(t *testing.T) {
	f := newFake()
	ctx, cancel := context.WithCancel(context.Background())
	a, err := open(ctx, testConfig(), testBackend(f))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err = a.ReadPacket(canceled, make([]byte, 1500)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.input <- []byte{1, 2, 3}
	if _, err = a.ReadPacket(context.Background(), make([]byte, 2)); !errors.Is(err, io.ErrShortBuffer) {
		t.Fatal(err)
	}
	f.input <- []byte{4, 5}
	buf := make([]byte, 1500)
	n, err := a.ReadPacket(context.Background(), buf)
	if err != nil || n != 2 || buf[0] != 4 {
		t.Fatalf("read: %d %v", n, err)
	}
	if _, err = a.WritePacket(canceled, []byte{7}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = a.WritePacket(context.Background(), make([]byte, 1501)); err == nil {
		t.Fatal("accepted oversize")
	}
	if _, err = a.WritePacket(context.Background(), []byte{8}); err != nil {
		t.Fatal(err)
	}
	if p := <-f.output; p[0] != 8 {
		t.Fatal(p)
	}
	done := make(chan error, 1)
	go func() { _, err := a.ReadPacket(context.Background(), buf); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("close left read blocked")
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestBlockedWriteCancellationDoesNotClose(t *testing.T) {
	f := newFake()
	f.blockWrite = true
	a, err := open(context.Background(), testConfig(), testBackend(f))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := a.WritePacket(ctx, []byte{1}); done <- err }()
	<-f.writeStarted
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-f.done:
		t.Fatal("individual cancellation closed native device")
	default:
	}
	ctx2, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if _, err = a.WritePacket(ctx2, []byte{2}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestCleanupErrorRetained(t *testing.T) {
	f := newFake()
	b := testBackend(f)
	boom := errors.New("remove failed")
	b.run = func(_ context.Context, args []string) error {
		for _, s := range args {
			if s == "del" {
				return boom
			}
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	a, err := open(ctx, testConfig(), b)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = a.Close(); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if err = a.Close(); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}
func TestRouteParsing(t *testing.T) {
	for _, tt := range []struct {
		platform, input string
		want            []string
	}{
		{"linux", `[{"dst":"default"},{"dst":"192.0.2.2"},{"dst":"198.51.100.0/24"}]`, []string{"192.0.2.2/32", "198.51.100.0/24"}},
		{"darwin", "Destination Gateway Flags Netif\ndefault 1.1.1.1 UG en0\n127 127.0.0.1 UCS lo0\n192.0.2.2 link#9 UH utun9\n", []string{"127.0.0.0/8", "192.0.2.2/32"}},
		{"windows", "Network Destination Netmask Gateway Interface Metric\n0.0.0.0 0.0.0.0 1.1.1.1 1.1.1.2 20\n192.0.2.2 255.255.255.255 On-link 1.1.1.2 30", []string{"0.0.0.0/0", "192.0.2.2/32"}},
	} {
		t.Run(tt.platform, func(t *testing.T) {
			routes, err := parseRoutes(tt.platform, []byte(tt.input))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range routes {
				if tt.platform != "darwin" && (r.Gateway != "" || r.Flags != "" || r.Netif != "") {
					t.Fatalf("unexpected metadata: %+v", r)
				}
				got = append(got, r.Prefix.String())
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestCancellationDuringConfigurationRollsBack(t *testing.T) {
	f := newFake()
	b := testBackend(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls [][]string
	b.run = func(callCtx context.Context, args []string) error {
		calls = append(calls, append([]string(nil), args...))
		if len(calls) == 2 {
			cancel()
			return nil
		}
		return callCtx.Err()
	}
	_, err := open(ctx, testConfig(), b)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if len(calls) != 4 || !reflect.DeepEqual(calls[3], plan("linux", "enettest", testConfig())[1].remove) {
		t.Fatalf("rollback missing: %v", calls)
	}
	select {
	case <-f.done:
	default:
		t.Fatal("native resource leaked")
	}
}
func TestReadWorkerAndQueueClose(t *testing.T) {
	f := newFake()
	a, err := open(context.Background(), testConfig(), testBackend(f))
	if err != nil {
		t.Fatal(err)
	}
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for range 100 {
			select {
			case f.input <- []byte{1}:
			case <-f.done:
				return
			}
		}
	}()
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("producer left blocked")
	}
}
func TestNameRules(t *testing.T) {
	for _, tt := range []struct {
		os, name string
		ok       bool
	}{
		{"linux", "tun0", true}, {"linux", "tun%d", false}, {"linux", "../tun", false},
		{"darwin", "utun", true}, {"darwin", "utun12", true}, {"darwin", "utun12junk", false}, {"darwin", "en0", false},
		{"windows", "Enet_Test", true}, {"windows", "a b", false},
	} {
		t.Run(tt.os+tt.name, func(t *testing.T) {
			c := testConfig()
			c.Name = tt.name
			_, err := validate(c, tt.os)
			if (err == nil) != tt.ok {
				t.Fatalf("name accepted=%v want %v: %v", err == nil, tt.ok, err)
			}
		})
	}
}

func TestLinkLocalConfiguration(t *testing.T) {
	for _, field := range []string{"local", "target", "both"} {
		t.Run(field, func(t *testing.T) {
			c := testConfig()
			if field != "target" {
				c.Local = netip.MustParsePrefix("169.254.8.1/32")
			}
			if field != "local" {
				c.Targets = []netip.Addr{netip.MustParseAddr("169.254.8.2")}
			}
			a, err := open(context.Background(), c, testBackend(newFake()))
			if err != nil {
				t.Fatalf("valid link-local configuration rejected: %v", err)
			}
			if err = a.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLinkLocalConflictsBeforeCreation(t *testing.T) {
	for _, field := range []string{"local", "target"} {
		for _, kind := range []string{"address", "route"} {
			t.Run(field+"/"+kind, func(t *testing.T) {
				c := testConfig()
				if field == "local" {
					c.Local = netip.MustParsePrefix("169.254.8.1/32")
				} else {
					c.Targets = []netip.Addr{netip.MustParseAddr("169.254.8.2")}
				}
				b := testBackend(newFake())
				b.create = func(string, int) (tun.Device, error) {
					t.Fatal("created conflicting link-local device")
					return nil, nil
				}
				if kind == "address" {
					b.interfaces = func() ([]net.Interface, error) { return []net.Interface{{Name: "management"}}, nil }
					b.addresses = func(net.Interface) ([]net.Addr, error) {
						return []net.Addr{&net.IPNet{IP: net.ParseIP("169.254.9.1"), Mask: net.CIDRMask(16, 32)}}, nil
					}
				} else {
					b.routes = func(context.Context) ([]routeEntry, error) {
						return []routeEntry{{Prefix: netip.MustParsePrefix("169.254.0.0/16")}}, nil
					}
				}
				_, err := open(context.Background(), c, b)
				if err == nil || !strings.Contains(err.Error(), "conflicts") {
					t.Fatalf("expected preflight conflict, got %v", err)
				}
			})
		}
	}
	c := testConfig()
	c.Local = netip.MustParsePrefix("169.254.8.1/32")
	c.Targets = []netip.Addr{c.Local.Addr()}
	if _, err := validate(c, "linux"); err == nil {
		t.Fatal("accepted target equal to link-local Local")
	}
}

func TestInvalidIPv4RemainsRejected(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "0.0.0.0", "0.1.2.3", "224.0.0.1", "255.255.255.255", "::1", "::ffff:169.254.8.1"} {
		t.Run(address, func(t *testing.T) {
			ip := netip.MustParseAddr(address)
			if validIP(ip) {
				t.Fatal("accepted invalid unicast address")
			}
			c := testConfig()
			c.Local = netip.PrefixFrom(ip, 32)
			if _, err := validate(c, "linux"); err == nil {
				t.Fatal("accepted invalid Local")
			}
			c = testConfig()
			c.Targets = []netip.Addr{ip}
			if _, err := validate(c, "linux"); err == nil {
				t.Fatal("accepted invalid target")
			}
		})
	}
}

func TestOpenAutoPreflightFallback(t *testing.T) {
	f := newFake()
	b := testBackend(f)
	b.routes = func(context.Context) ([]routeEntry, error) {
		return []routeEntry{{Prefix: netip.MustParsePrefix("198.18.0.0/15")}}, nil
	}
	creates := 0
	b.create = func(string, int) (tun.Device, error) { creates++; return f, nil }
	a, err := openAuto(t.Context(), []netip.Addr{netip.MustParseAddr("10.99.0.1")}, b)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if creates != 1 || a.Local() != netip.MustParsePrefix("192.0.2.1/32") {
		t.Fatalf("creates=%d local=%v", creates, a.Local())
	}
}

func TestOpenAutoDoesNotRetryNativeFailure(t *testing.T) {
	for _, configure := range []bool{false, true} {
		t.Run(fmt.Sprint(configure), func(t *testing.T) {
			f := newFake()
			b := testBackend(f)
			creates := 0
			failure := errors.New("permission denied")
			b.create = func(string, int) (tun.Device, error) {
				creates++
				if !configure {
					return nil, failure
				}
				return f, nil
			}
			if configure {
				b.run = func(context.Context, []string) error { return failure }
			}
			_, err := openAuto(t.Context(), []netip.Addr{netip.MustParseAddr("10.99.0.1")}, b)
			if !errors.Is(err, failure) || creates != 1 {
				t.Fatalf("creates=%d err=%v", creates, err)
			}
		})
	}
}

func TestDarwinRouteMetadataAndDuplicates(t *testing.T) {
	input := "Routing tables\n\nInternet:\nDestination        Gateway            Flags        Netif Expire\ndefault            192.168.1.1        UGScg        en0\n169.254            link#14            UCS          en0      !\n169.254            link#14            UCS          en0      !\n169.254.91.3       link#22            UHLWIi       utun8    12\n"
	routes, err := parseRoutes("darwin", []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	want := []routeEntry{
		{Prefix: netip.MustParsePrefix("169.254.0.0/16"), Gateway: "link#14", Flags: "UCS", Netif: "en0"},
		{Prefix: netip.MustParsePrefix("169.254.0.0/16"), Gateway: "link#14", Flags: "UCS", Netif: "en0"},
		{Prefix: netip.MustParsePrefix("169.254.91.3/32"), Gateway: "link#22", Flags: "UHLWIi", Netif: "utun8"},
	}
	if !reflect.DeepEqual(routes, want) {
		t.Fatalf("routes=%+v want=%+v", routes, want)
	}
}

func TestDarwinRouteMissingMetadataRejected(t *testing.T) {
	for _, line := range []string{"169.254", "169.254 link#14", "169.254 link#14 UCS", "default", "default 192.168.1.1 UG"} {
		t.Run(line, func(t *testing.T) {
			if routes, err := parseRoutes("darwin", []byte("Destination Gateway Flags Netif\n"+line+"\n")); err == nil || routes != nil {
				t.Fatalf("accepted incomplete route: %+v %v", routes, err)
			}
		})
	}
}

func TestDarwinGenericLinkLocalStillConflicts(t *testing.T) {
	f := newFake()
	b := testBackend(f)
	b.os = "darwin"
	b.routes = func(context.Context) ([]routeEntry, error) {
		return parseRoutes("darwin", []byte("169.254 link#14 UCS en0\n"))
	}
	b.create = func(string, int) (tun.Device, error) { t.Fatal("created conflicting device"); return nil, nil }
	_, err := open(t.Context(), Config{Name: "utun", Local: netip.MustParsePrefix("192.0.2.1/32"), Targets: []netip.Addr{netip.MustParseAddr("169.254.91.3")}}, b)
	if err == nil {
		t.Fatal("accepted generic link-local conflict")
	}
	for _, part := range []string{"169.254.0.0/16", "en0", "UCS", "link#14"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("missing %q in %v", part, err)
		}
	}
}

const darwinFallbackFixture = "Destination Gateway Flags Netif Expire\n169.254 link#30 UCS en7 !\n169.254 link#15 UCSI en0 !\n169.254.255.255 link#30 UHLSW en7 !\n"

func fallbackBackend(t *testing.T, f *fakeNative) backend {
	t.Helper()
	b := testBackend(f)
	b.os = "darwin"
	b.interfaces = func() ([]net.Interface, error) {
		return []net.Interface{{Index: 30, Name: "en7", Flags: net.FlagUp | net.FlagBroadcast}, {Index: 15, Name: "en0", Flags: net.FlagUp | net.FlagBroadcast}}, nil
	}
	b.routes = func(context.Context) ([]routeEntry, error) {
		return parseRoutes("darwin", []byte(darwinFallbackFixture))
	}
	return b
}
func fallbackConfig() Config {
	return Config{Name: "utun", MTU: 1500, Local: netip.MustParsePrefix("192.0.2.1/32"), Targets: []netip.Addr{netip.MustParseAddr("169.254.91.3")}}
}

func TestDarwinObservedFallbackAndOwnedRoutes(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			f := newFake()
			b := fallbackBackend(t, f)
			rows, err := b.routes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 3 || rows[1].Flags != "UCSI" || rows[2].Prefix != netip.MustParsePrefix("169.254.255.255/32") {
				t.Fatal(rows)
			}
			rows = append(rows, rows[0])
			if reverse {
				for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
					rows[i], rows[j] = rows[j], rows[i]
				}
			}
			b.routes = func(context.Context) ([]routeEntry, error) { return rows, nil }
			var commands []string
			b.run = func(_ context.Context, args []string) error {
				commands = append(commands, strings.Join(args, " "))
				return nil
			}
			c := fallbackConfig()
			a, err := open(t.Context(), c, b)
			if err != nil {
				t.Fatal(err)
			}
			if err = a.Close(); err != nil {
				t.Fatal(err)
			}
			ops := plan("darwin", "enettest", c)
			want := []string{strings.Join(ops[0].add, " "), strings.Join(ops[1].add, " "), strings.Join(ops[1].remove, " "), strings.Join(ops[0].remove, " ")}
			if !reflect.DeepEqual(commands, want) {
				t.Fatalf("commands=%v want=%v", commands, want)
			}
		})
	}
}

func TestDarwinFallbackRejectionBoundaries(t *testing.T) {
	base := routeEntry{Prefix: netip.MustParsePrefix("169.254.0.0/16"), Gateway: "link#30", Flags: "UCS", Netif: "en7"}
	iface := net.Interface{Index: 30, Name: "en7", Flags: net.FlagUp | net.FlagBroadcast}
	target := netip.MustParseAddr("169.254.91.3")
	type fallbackCase struct {
		name, platform string
		route          routeEntry
		iface          net.Interface
		target         netip.Addr
	}
	tests := []fallbackCase{
		{"valid", "darwin", base, iface, target},
	}
	for _, platform := range []string{"linux", "windows"} {
		tests = append(tests, fallbackCase{platform, platform, base, iface, target})
	}
	for _, flags := range []string{"", "UC", "UCSII", "UUCS", "UCSg", "UCSG", "UCSH", "UCSR", "UCSB", "UCSW"} {
		r := base
		r.Flags = flags
		tests = append(tests, fallbackCase{"route flags " + flags, "darwin", r, iface, target})
	}
	for _, gateway := range []string{"192.168.1.1", "link#0", "link#-30", "link#+30", "link#31", "link#30x", "link#"} {
		r := base
		r.Gateway = gateway
		tests = append(tests, fallbackCase{"gateway " + gateway, "darwin", r, iface, target})
	}
	for _, name := range []string{"utun7", "en7x", "en", "lo0", "en8"} {
		r := base
		r.Netif = name
		tests = append(tests, fallbackCase{"netif " + name, "darwin", r, iface, target})
	}
	for _, flags := range []net.Flags{net.FlagUp, net.FlagBroadcast, net.FlagUp | net.FlagBroadcast | net.FlagLoopback, net.FlagUp | net.FlagBroadcast | net.FlagPointToPoint} {
		i := iface
		i.Flags = flags
		tests = append(tests, fallbackCase{fmt.Sprint("interface flags ", flags), "darwin", base, i, target})
	}
	for _, prefix := range []string{"169.254.91.0/24", "169.254.91.3/32", "169.0.0.0/8"} {
		r := base
		r.Prefix = netip.MustParsePrefix(prefix)
		tests = append(tests, fallbackCase{prefix, "darwin", r, iface, target})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := darwinLinkLocalFallback(test.platform, test.route, test.target, []net.Interface{test.iface})
			if got != (test.name == "valid") {
				t.Fatal("unexpected fallback classification", got)
			}
		})
	}
	if darwinLinkLocalFallback("darwin", base, netip.MustParseAddr("192.0.2.2"), []net.Interface{iface}) || darwinLinkLocalFallback("darwin", base, target, nil) {
		t.Fatal("accepted non-link-local or missing interface")
	}
}

func TestDarwinFallbackDoesNotHideOtherConflicts(t *testing.T) {
	for _, kind := range []string{"host", "subnet", "vpn", "address", "local"} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprint(kind, reverse), func(t *testing.T) {
				b := fallbackBackend(t, newFake())
				c := fallbackConfig()
				rows, err := b.routes(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "host":
					rows = append(rows, routeEntry{Prefix: netip.PrefixFrom(c.Targets[0], 32), Gateway: "link#30", Flags: "UHLWI", Netif: "en7"})
				case "subnet":
					rows = append(rows, routeEntry{Prefix: netip.MustParsePrefix("169.254.91.0/24")})
				case "vpn":
					rows = append(rows, routeEntry{Prefix: netip.MustParsePrefix("169.254.0.0/16"), Gateway: "link#30", Flags: "UCS", Netif: "utun9"})
				case "address":
					b.addresses = func(net.Interface) ([]net.Addr, error) {
						return []net.Addr{&net.IPNet{IP: net.ParseIP("169.254.20.1"), Mask: net.CIDRMask(16, 32)}}, nil
					}
				case "local":
					c.Local = netip.MustParsePrefix("169.254.20.1/32")
				}
				if reverse {
					for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
						rows[i], rows[j] = rows[j], rows[i]
					}
				}
				b.routes = func(context.Context) ([]routeEntry, error) { return rows, nil }
				b.create = func(string, int) (tun.Device, error) { t.Fatal("created conflicting device"); return nil, nil }
				if _, err := open(t.Context(), c, b); err == nil {
					t.Fatal("accepted conflict")
				}
			})
		}
	}
}

func TestDarwinFallbackRollbackOnlySuccessfulAdds(t *testing.T) {
	for _, fail := range []int{1, 2} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			f := newFake()
			b := fallbackBackend(t, f)
			c := fallbackConfig()
			c.Targets = append(c.Targets, netip.MustParseAddr("169.254.91.4"))
			ops := plan("darwin", "enettest", c)
			var commands []string
			calls := 0
			failure := errors.New("route add: file exists")
			b.run = func(_ context.Context, args []string) error {
				commands = append(commands, strings.Join(args, " "))
				calls++
				if calls == fail+1 {
					return failure
				}
				return nil
			}
			if _, err := open(t.Context(), c, b); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			var want []string
			for i := 0; i <= fail; i++ {
				want = append(want, strings.Join(ops[i].add, " "))
			}
			for i := fail - 1; i >= 0; i-- {
				want = append(want, strings.Join(ops[i].remove, " "))
			}
			if !reflect.DeepEqual(commands, want) {
				t.Fatalf("commands=%v want=%v", commands, want)
			}
		})
	}
}
