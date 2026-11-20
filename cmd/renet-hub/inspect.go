package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"remote-enet/internal/hubauth"
	"remote-enet/internal/identity"
	"remote-enet/internal/ticket"
)

func inspect(out io.Writer, cfg hubauth.Config, fp identity.Fingerprint, path, device string) error {
	if path == "" {
		return errors.New("--ticket-file is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("ticket-file: %w", err)
	}
	defer f.Close()
	const maxFile = 64 << 10
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return fmt.Errorf("ticket-file: %w", err)
	}
	if len(b) > maxFile {
		return errors.New("ticket-file: exceeds 64 KiB")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(string(bytes.TrimSpace(b)))
	if err != nil {
		return errors.New("ticket-file: expected unpadded base64url")
	}
	var headerKid *ticket.Kid
	kid, c, err := ticket.Verify(raw, func(k ticket.Kid) (ed25519.PublicKey, bool) {
		headerKid = &k
		for _, issuer := range cfg.Issuers {
			pub, err := base64.RawURLEncoding.DecodeString(issuer.PublicKey)
			if err == nil && ticket.KidOf(pub) == k {
				return pub, true
			}
		}
		return nil, false
	})
	if err != nil {
		if headerKid != nil {
			return fmt.Errorf("%w (unverified kid=%x)", err, *headerKid)
		}
		return err
	}
	fmt.Fprintf(out, "kid=%x jti=%s hub=%s role=%d bearer=%t iat=%s nbf=%s exp=%s\n", kid, base64.RawURLEncoding.EncodeToString(c.JTI[:]), c.Hub.Short(), c.Role, c.Bearer, c.IssuedAt.UTC().Format(time.RFC3339), c.NotBefore.UTC().Format(time.RFC3339), c.Expires.UTC().Format(time.RFC3339))
	if !c.Device.IsZero() {
		fmt.Fprintln(out, "device="+c.Device.Short())
	}
	if !c.Peer.IsZero() {
		fmt.Fprintln(out, "peer="+c.Peer.Short())
	}
	if !c.PairUntil.IsZero() {
		fmt.Fprintln(out, "pair_until="+c.PairUntil.UTC().Format(time.RFC3339))
	}
	dev := c.Device
	if device != "" {
		dev, err = identity.ParseFingerprint(device)
		if err != nil || dev.IsZero() {
			return errors.New("--device: expected full nonzero fingerprint")
		}
	} else {
		fmt.Fprintln(out, "Skipped A7 outer TLS device comparison (no --device).")
		if c.Bearer {
			fmt.Fprintln(out, "Skipped A11 device deny list for unidentified bearer.")
		}
	}
	fmt.Fprintln(out, "Skipped A8 outer route comparison (offline).")
	if err := hubauth.Explain(cfg, fp, raw, dev, time.Now()); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "Passed offline checks.")
	return err
}
