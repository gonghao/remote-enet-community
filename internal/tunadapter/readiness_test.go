package tunadapter

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/tun"
)

func TestAddressReadyIdentityAndState(t *testing.T) {
	local := netip.MustParsePrefix("198.18.8.1/32")
	preferred := addressObservation{luid: 25, prefix: local, up: true, dad: 4}
	for _, tt := range []struct {
		name        string
		rows        []addressObservation
		ready, fail bool
	}{
		{name: "missing"},
		{"preferred", []addressObservation{preferred}, true, false},
		{"foreign interface", []addressObservation{{luid: 26, prefix: local, up: true, dad: 4}}, false, false},
		{"foreign address", []addressObservation{{luid: 25, prefix: netip.MustParsePrefix("198.18.8.2/32"), up: true, dad: 4}}, false, false},
		{"down", []addressObservation{{luid: 25, prefix: local, up: false, dad: 4}}, false, false},
		{"tentative", []addressObservation{{luid: 25, prefix: local, up: true, dad: 1}}, false, false},
		{"prefix changed", []addressObservation{{luid: 25, prefix: netip.MustParsePrefix("198.18.8.1/24"), up: true, dad: 4}}, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ready, err := addressReady(25, local, tt.rows)
			if ready != tt.ready || (err != nil) != tt.fail {
				t.Fatalf("ready=%v error=%v", ready, err)
			}
		})
	}
	for _, dad := range []int32{0, 2, 3, 99} {
		row := preferred
		row.dad = dad
		if _, err := addressReady(25, local, []addressObservation{row}); err == nil {
			t.Fatalf("accepted DAD state %d", dad)
		}
	}
}
func TestWaitAddressReadyTransitions(t *testing.T) {
	local := netip.MustParsePrefix("198.18.8.1/32")
	calls, pauses := 0, 0
	probe := func(context.Context) ([]addressObservation, error) {
		calls++
		switch calls {
		case 1:
			return nil, nil
		case 2:
			return []addressObservation{{luid: 26, prefix: local, up: true, dad: 4}, {luid: 25, prefix: local, up: true, dad: 1}}, nil
		default:
			return []addressObservation{{luid: 25, prefix: local, up: true, dad: 4}}, nil
		}
	}
	pause := func(context.Context) error { pauses++; return nil }
	if err := waitAddressReady(context.Background(), 25, local, time.Second, probe, pause); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || pauses != 2 {
		t.Fatalf("queries=%d pauses=%d", calls, pauses)
	}
}
func TestWaitAddressReadyCancellationAndErrors(t *testing.T) {
	local := netip.MustParsePrefix("198.18.8.1/32")
	boom := errors.New("query failure")
	for _, kind := range []string{"already canceled", "during query", "during wait", "timeout", "query failure", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := error(context.Canceled)
			budget := time.Second
			if kind == "already canceled" {
				cancel()
			}
			if kind == "timeout" {
				budget = 10 * time.Millisecond
				want = context.DeadlineExceeded
			}
			if kind == "query failure" {
				want = boom
			}
			if kind == "duplicate" {
				want = nil
			}
			probe := func(context.Context) ([]addressObservation, error) {
				switch kind {
				case "already canceled":
					t.Fatal("queried canceled context")
				case "during query":
					cancel()
					return []addressObservation{{luid: 25, prefix: local, up: true, dad: 4}}, nil
				case "query failure":
					return nil, boom
				case "duplicate":
					return []addressObservation{{luid: 25, prefix: local, up: true, dad: 2}}, nil
				}
				return []addressObservation{{luid: 25, prefix: local, up: true, dad: 1}}, nil
			}
			pause := func(ctx context.Context) error {
				if kind == "during wait" {
					cancel()
				}
				if kind == "duplicate" || kind == "query failure" {
					t.Fatal("waited after terminal failure")
				}
				<-ctx.Done()
				return ctx.Err()
			}
			err := waitAddressReady(ctx, 25, local, budget, probe, pause)
			if kind == "duplicate" {
				if err == nil || !strings.Contains(err.Error(), "DAD state 2") {
					t.Fatal(err)
				}
			} else if !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
		})
	}
}
func TestReadinessFailureRollsBackAllOwnedResources(t *testing.T) {
	for _, kind := range []string{"state failure", "cancellation"} {
		t.Run(kind, func(t *testing.T) {
			f := newFake()
			var log []string
			f.log = &log
			b := testBackend(f)
			b.os = "windows"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			boom := errors.New("DAD duplicate")
			want := boom
			b.run = func(ctx context.Context, args []string) error {
				if err := ctx.Err(); err != nil {
					t.Fatalf("cleanup inherited canceled context: %v", err)
				}
				log = append(log, strings.Join(args, " "))
				return nil
			}
			b.ready = func(readyCtx context.Context, device tun.Device, local netip.Prefix) error {
				if device != f || local != testConfig().Local {
					t.Fatal("readiness identity changed")
				}
				c := testConfig()
				c.MTU = 1500
				ops := plan("windows", "enettest", c)
				if len(log) != len(ops) {
					t.Fatal("readiness ran before configuration completed")
				}
				// Events must remain consumed while readiness is pending.
				select {
				case f.events <- tun.EventUp:
				case <-time.After(time.Second):
					t.Fatal("Events blocked during readiness")
				}
				if kind == "cancellation" {
					cancel()
					return readyCtx.Err()
				}
				return boom
			}
			if kind == "cancellation" {
				want = context.Canceled
			}
			_, err := open(ctx, testConfig(), b)
			if !errors.Is(err, want) {
				t.Fatalf("failure lost: %v", err)
			}
			c := testConfig()
			c.MTU = 1500
			ops := plan("windows", "enettest", c)
			var expected []string
			for _, op := range ops {
				expected = append(expected, strings.Join(op.add, " "))
			}
			for i := len(ops) - 1; i >= 0; i-- {
				if len(ops[i].remove) > 0 {
					expected = append(expected, strings.Join(ops[i].remove, " "))
				}
			}
			expected = append(expected, "native close")
			if !reflect.DeepEqual(log, expected) {
				t.Fatalf("cleanup got %v want %v", log, expected)
			}
			select {
			case <-f.done:
			default:
				t.Fatal("native leaked")
			}
			if _, err = f.Read(nil, nil, 0); !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
		})
	}
}

func TestAddressReadyAggregatesMatchingEntries(t *testing.T) {
	local := netip.MustParsePrefix("198.18.8.1/32")
	preferred := addressObservation{luid: 25, prefix: local, up: true, dad: dadPreferred}
	for _, tt := range []struct {
		name        string
		other       addressObservation
		ready, fail bool
	}{
		{"tentative", addressObservation{luid: 25, prefix: local, up: true, dad: dadTentative}, true, false},
		{"preferred down", addressObservation{luid: 25, prefix: local, up: false, dad: dadPreferred}, true, false},
		{"invalid", addressObservation{luid: 25, prefix: local, up: true, dad: dadInvalid}, false, true},
		{"duplicate", addressObservation{luid: 25, prefix: local, up: true, dad: dadDuplicate}, false, true},
		{"deprecated", addressObservation{luid: 25, prefix: local, up: true, dad: dadDeprecated}, false, true},
		{"unknown", addressObservation{luid: 25, prefix: local, up: true, dad: 99}, false, true},
		{"prefix changed", addressObservation{luid: 25, prefix: netip.MustParsePrefix("198.18.8.1/24"), up: true, dad: dadPreferred}, false, true},
		{"foreign duplicate", addressObservation{luid: 26, prefix: local, up: true, dad: dadDuplicate}, true, false},
	} {
		for _, reverse := range []bool{false, true} {
			order := "preferred first"
			rows := []addressObservation{preferred, tt.other}
			if reverse {
				order = "preferred last"
				rows[0], rows[1] = rows[1], rows[0]
			}
			t.Run(tt.name+"/"+order, func(t *testing.T) {
				ready, err := addressReady(25, local, rows)
				if ready != tt.ready || (err != nil) != tt.fail {
					t.Fatalf("ready=%v error=%v; want ready=%v fail=%v", ready, err, tt.ready, tt.fail)
				}
			})
		}
	}
	rows := []addressObservation{{luid: 25, prefix: local, up: true, dad: dadTentative}, {luid: 25, prefix: local, up: false, dad: dadPreferred}}
	if ready, err := addressReady(25, local, rows); ready || err != nil {
		t.Fatalf("tentative/down must wait: ready=%v error=%v", ready, err)
	}
}
