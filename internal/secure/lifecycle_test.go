package secure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// Leave the lifecycle watcher and periodic ping stopped so each automatic
// closer can be released after cancellation, without racing the ctx watcher.
func lifecycleSession(t *testing.T) (*session, net.Conn) {
	t.Helper()
	x, y := net.Pipe()
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.LogOutput = io.Discard
	mux, err := yamux.Client(x, cfg)
	if err != nil {
		x.Close()
		y.Close()
		t.Fatal(err)
	}
	s := &session{mux: mux, raw: x, done: make(chan struct{})}
	t.Cleanup(func() { y.Close(); s.Close(nil) })
	return s, y
}

func TestAutomaticClosePreservesLifetimeCancellation(t *testing.T) {
	for _, source := range []string{"mux", "ping"} {
		for _, termination := range []string{"cancel", "deadline"} {
			t.Run(source+"/"+termination, func(t *testing.T) {
				ctx, cancel := context.WithCancel(deadline(t))
				want := context.Canceled
				if termination == "deadline" {
					cancel()
					ctx, cancel = context.WithDeadline(deadline(t), time.Now().Add(-time.Second))
					want = context.DeadlineExceeded
				}
				defer cancel()
				s, peer := lifecycleSession(t)
				observed := make(chan error, 1)
				release := make(chan struct{})
				finished := make(chan error, 1)
				// Also release the closer if an assertion fails before the barrier.
				t.Cleanup(func() {
					select {
					case <-release:
					default:
						close(release)
					}
				})
				go func() {
					var err error
					if source == "mux" {
						<-s.mux.CloseChan()
						err = io.EOF
					} else {
						_, err = s.mux.Ping()
						if err != nil {
							err = fmt.Errorf("secure: ping: %w", err)
						}
					}
					observed <- err
					<-release
					s.closeOnError(ctx, err)
					finished <- s.Err()
				}()
				peer.Close()
				if err := awaitError(t, observed); err == nil {
					t.Fatal("peer close did not produce a transport failure")
				}
				cancel()
				if !errors.Is(ctx.Err(), want) {
					t.Fatalf("lifetime error = %v, want %v", ctx.Err(), want)
				}
				close(release)
				if err := awaitError(t, finished); !errors.Is(err, want) {
					t.Fatalf("terminal error = %v, want %v", err, want)
				}
			})
		}
	}
}

func TestAutomaticClosePreservesTransportFailure(t *testing.T) {
	for _, reason := range []error{io.EOF, fmt.Errorf("secure: ping: %w", yamux.ErrTimeout)} {
		t.Run(reason.Error(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(deadline(t))
			defer cancel()
			s, _ := lifecycleSession(t)
			s.closeOnError(ctx, reason)
			<-s.Done()
			cancel()
			s.closeOnError(ctx, io.EOF)
			if s.Err() != reason {
				t.Fatalf("terminal error = %v, want original %v", s.Err(), reason)
			}
		})
	}
}

func TestRemoteCloseDoesNotReportLifetimeCancellation(t *testing.T) {
	a, b := pair(t, false)
	if err := b.Close(errors.New("remote close")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Done():
	case <-deadline(t).Done():
		t.Fatal("remote close did not terminate session")
	}
	if err := a.Err(); err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("remote close misclassified: %v", err)
	}
}

func TestExplicitClosePreservesReasonAfterCancellation(t *testing.T) {
	for _, reason := range []error{nil, errors.New("explicit close")} {
		name := "nil"
		if reason != nil {
			name = reason.Error()
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(deadline(t))
			defer cancel()
			s, _ := lifecycleSession(t)
			cancel()
			if err := s.Close(reason); err != nil {
				t.Fatal(err)
			}
			<-s.Done()
			s.closeOnError(ctx, context.Canceled)
			want := reason
			if want == nil {
				want = io.EOF
			}
			if s.Err() != want {
				t.Fatalf("terminal error = %v, want explicit %v", s.Err(), want)
			}
		})
	}
}
