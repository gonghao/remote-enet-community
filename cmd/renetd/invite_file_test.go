package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/transport"
)

func TestClientInvitationFileUTF8(t *testing.T) {
	peer := identity.Fingerprint{1}
	code, err := (invite.Invite{Version: 1, Peer: transport.PeerInfo{ID: peer, Endpoints: []transport.Endpoint{{Kind: "hub", Addr: "127.0.0.1:47000", Hub: peer}}}, Token: [16]byte{1}, ExpiresAt: time.Now().Add(time.Minute)}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, bom := range []string{"", "\uFEFF"} {
		t.Run(map[bool]string{false: "plain", true: "powershell-bom"}[bom != ""], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "invite.txt")
			if err := os.WriteFile(path, []byte(bom+code+"\r\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// The absent interface now waits. Successful preparation is logged
			// after decoding; no discovery or external connection is attempted.
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			err := runClient(ctx, []string{"--state-dir", filepath.Join(dir, "state"), "--invite-file", path, "--nic", "remote-enet-test-interface-absent"}, io.Discard, io.Discard, logger)
			if !errors.Is(err, context.DeadlineExceeded) || !bytes.Contains(logs.Bytes(), []byte("client ready")) {
				t.Fatalf("UTF-8 invitation did not reach NIC waiting: %v", err)
			}
		})
	}
}
