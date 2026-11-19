package hub

import (
	"errors"
	"net"
	"testing"

	router "remote-enet/internal/hub"
	"remote-enet/internal/transport"
)

func TestDialAfterClose(t *testing.T) {
	local := id(t)
	connector, acceptor, err := New(Options{
		Local: local, Embedded: router.NewEmbedded(), Wrap: stubWrap(local.Fingerprint()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := acceptor.Close(); err != nil {
		t.Fatal(err)
	}
	if session, err := connector.Dial(t.Context(), transport.PeerInfo{ID: local.Fingerprint()}); session != nil || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Dial after Close: %v", err)
	}
	if session, _, err := connector.DialBootstrap(t.Context(), "127.0.0.1:0"); session != nil || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("DialBootstrap after Close: %v", err)
	}
}
