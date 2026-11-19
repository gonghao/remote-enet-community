package invite

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// InputKind identifies which connection flow may use the parsed input.
type InputKind uint8

const (
	// Raw carries a decoded renet1 invitation.
	Raw InputKind = iota + 1
	// Direct carries an address and an eight-digit pairing code.
	Direct
	// Center is reserved for pairing with a code alone.
	Center
)

// Input holds secrets for Raw and Direct. Center is reserved and retains no code.
type Input struct {
	Kind    InputKind
	Invite  Invite
	Address string
	Code    string
}

// Parse errors use fixed text so untrusted input never reaches diagnostics.
var (
	ErrInvalidInvite = errors.New("Invalid invite")
	ErrInvalidHost   = errors.New("Invalid host")
	ErrInvalidPort   = errors.New("Invalid port")
	ErrInvalidCode   = errors.New("Enter eight digits")
)

// ErrInviteExpired also matches ErrInvalidInvite through errors.Is.
var ErrInviteExpired error = expiredInviteError{}

type expiredInviteError struct{}

func (expiredInviteError) Error() string { return "Invitation expired" }
func (expiredInviteError) Unwrap() error { return ErrInvalidInvite }

// ParseInput canonicalizes direct addresses and accepts Unicode whitespace between fields.
func ParseInput(s string) (Input, error) {
	// Copied text can start with a BOM that TrimSpace does not remove.
	s = strings.TrimSpace(strings.TrimPrefix(s, "\ufeff"))
	const prefix = "renet1:"
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		i, err := Decode(s)
		if err != nil {
			if errors.Is(err, ErrInviteExpired) {
				return Input{}, ErrInviteExpired
			}
			return Input{}, ErrInvalidInvite
		}
		return Input{Kind: Raw, Invite: i}, nil
	}

	fields := strings.Fields(s)
	if _, ok := parseCode(fields); ok {
		return Input{Kind: Center}, nil
	}
	if len(fields) == 0 || len(fields) > 3 {
		return Input{}, ErrInvalidCode
	}
	address, err := ParseAddress(fields[0])
	if err != nil {
		return Input{}, err
	}
	code, ok := parseCode(fields[1:])
	if !ok {
		return Input{}, ErrInvalidCode
	}
	return Input{Kind: Direct, Address: address, Code: code}, nil
}

func parseCode(fields []string) (string, bool) {
	if len(fields) == 0 || len(fields) > 2 {
		return "", false
	}
	var digits [8]byte
	width := len(digits) / len(fields)
	for i, field := range fields {
		n := 0
		for _, r := range field {
			if r >= '０' && r <= '９' {
				r = r - '０' + '0'
			}
			if r < '0' || r > '9' || n == width {
				return "", false
			}
			digits[i*width+n] = byte(r)
			n++
		}
		if n != width {
			return "", false
		}
	}
	return string(digits[:]), true
}

// ParseAddress returns the canonical form of a direct share address.
func ParseAddress(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return "", ErrInvalidHost
	}
	bracketed := strings.HasPrefix(address, "[")
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Is6() != bracketed || ip.Zone() != "" || ip.Unmap().IsUnspecified() {
			return "", ErrInvalidHost
		}
		host = ip.String()
	} else {
		if bracketed || !validDNS(host) {
			return "", ErrInvalidHost
		}
		host = strings.ToLower(host)
	}

	for i := range len(port) {
		if port[i] < '0' || port[i] > '9' {
			return "", ErrInvalidPort
		}
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return "", ErrInvalidPort
	}
	return net.JoinHostPort(host, strconv.FormatUint(n, 10)), nil
}

func validDNS(host string) bool {
	name := strings.TrimSuffix(host, ".")
	if len(name) > 253 {
		return false
	}
	numeric := true
	for label := range strings.SplitSeq(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			switch b := label[i]; {
			case b >= '0' && b <= '9':
			case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b == '-':
				numeric = false
			default:
				return false
			}
		}
	}
	return !numeric
}
