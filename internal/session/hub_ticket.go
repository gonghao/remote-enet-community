package session

import (
	"fmt"
	"slices"

	"remote-enet/internal/proto"
	"remote-enet/internal/ticket"
)

// HubTicket returns a copy of the device ticket received during this handshake,
// or nil when no ticket was received.
func (s *Session) HubTicket() []byte { return slices.Clone(s.hubTicket) }

func (s *Session) exchangeHubTicket() error {
	if s.opts.Role == RoleOperator {
		return s.sendHubTicket()
	}

	msg, err := proto.ReadHubTicket(s.control)
	if err != nil {
		return pairingFailure(ReasonHubTicket)
	}
	_, c, err := ticket.Inspect(msg.Ticket)
	const dial = 1
	if err != nil || c.Role != dial || c.Bearer || c.Device != s.opts.LocalFP || c.Peer != s.Peer() || c.Hub != s.opts.EndpointHub || !c.Expires.After(s.opts.Now()) {
		return pairingFailure(ReasonHubTicket)
	}
	s.hubTicket = msg.Ticket
	return nil
}

func (s *Session) sendHubTicket() error {
	client := s.Peer()
	raw, err := s.opts.GrantTicket(client)
	var claims ticket.Claims
	if err == nil {
		_, claims, err = ticket.Inspect(raw)
	}
	if err != nil {
		// Issuer errors can contain key paths or ticket material.
		s.opts.Logger.Error("hub ticket issuance failed", "client", client.Short())
		s.finishRejection()
		return pairingFailure(ReasonHubTicket)
	}
	if err := proto.Write(s.control, proto.HubTicket{Ticket: raw}); err != nil {
		return pairingFailure(ReasonHubTicket)
	}
	s.opts.Logger.Info("issued hub ticket", "client", client.Short(), "jti", fmt.Sprintf("%x", claims.JTI), "exp", claims.Expires.Unix())
	return nil
}
