package center

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/fxamacker/cbor/v2"
)

// Request contains the bytes shared by transport and signature verification.
// Target must be the canonical origin-form target actually sent.
type Request struct {
	CenterID, Method, Target string
	Body                     []byte
}

// RequestAuth carries caller-selected values. Timestamp is corrected Unix time.
type RequestAuth struct {
	Timestamp      int64
	Client         string
	Nonce          []byte
	IdempotencyKey string
}

var (
	// ErrBadRequest identifies malformed signing headers or fields.
	ErrBadRequest = errors.New("bad_request")
	// ErrUnauthenticated identifies a well-formed signature that failed verification.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrClockSkew identifies a timestamp outside the allowed request window.
	ErrClockSkew       = errors.New("clock_skew")
	idempotencyPattern = regexp.MustCompile("^[A-Za-z0-9_-]{16,64}$")
	wire64             = base64.RawURLEncoding.Strict()
)

// RequestWindow is the inclusive request timestamp tolerance in seconds.
const RequestWindow int64 = 60

// CheckTimestamp compares Unix seconds without overflowing at the int64 limits.
func CheckTimestamp(serverNow, ts, window int64) error {
	if serverNow < ts {
		serverNow, ts = ts, serverNow
	}
	if window < 0 || uint64(serverNow)-uint64(ts) > uint64(window) {
		return ErrClockSkew
	}
	return nil
}

// SigningInput uses byte strings for the target, principal, nonce and digest.
// The caller supplies validated fields; a nil nonce encodes as an empty bstr.
func SigningInput(r Request, a RequestAuth, kind string, principal []byte) []byte {
	digest := sha256.Sum256(r.Body)
	nonce := a.Nonce
	if nonce == nil {
		nonce = []byte{}
	}
	// For this fixed array of strings, bytes and one integer the default
	// encoder produces RFC 8949 core deterministic encoding.
	b, err := cbor.Marshal([]any{"renet-center/v1 request", r.CenterID, kind, principal, r.Method, []byte(r.Target), a.Timestamp, a.Client, nonce, a.IdempotencyKey, digest[:]})
	if err != nil {
		panic(err)
	}
	return b
}

func (a RequestAuth) validate(r Request) error {
	if a.Timestamp < 0 {
		return fmt.Errorf("%w: negative timestamp", ErrBadRequest)
	}
	if r.Method == "" || r.Method != strings.ToUpper(r.Method) {
		return fmt.Errorf("%w: invalid method", ErrBadRequest)
	}
	if !centerIDPattern.MatchString(r.CenterID) {
		return fmt.Errorf("%w: invalid center ID", ErrBadRequest)
	}
	role, version, _ := strings.Cut(a.Client, "/")
	validRole := role == "operator" || role == "client" || role == "hub"
	if !validRole || !semverPattern.MatchString(version) || (len(a.Nonce) != 0 && len(a.Nonce) != 16) {
		return fmt.Errorf("%w: invalid client or nonce", ErrBadRequest)
	}
	path, _, _ := strings.Cut(r.Target, "?")
	required := r.Method == "POST" && path != "/v1/node/heartbeat"
	if (required || a.IdempotencyKey != "") && !idempotencyPattern.MatchString(a.IdempotencyKey) {
		return fmt.Errorf("%w: invalid idempotency key", ErrBadRequest)
	}
	return nil
}

const deviceHeader = "Renet-Device-Key"
const nodeHeader = "Renet-Node-Id"

// SignDevice derives the principal from the signing key.
func SignDevice(r Request, a RequestAuth, key ed25519.PrivateKey) (http.Header, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid device key length")
	}
	return signHeaders(r, a, key, "device", deviceHeader, key.Public().(ed25519.PublicKey))
}

// SignNode binds the assigned node ID to the request.
func SignNode(r Request, a RequestAuth, key ed25519.PrivateKey, id [16]byte) (http.Header, error) {
	return signHeaders(r, a, key, "node", nodeHeader, id[:])
}

func signHeaders(r Request, a RequestAuth, key ed25519.PrivateKey, kind, header string, principal []byte) (http.Header, error) {
	target, err := CanonicalTarget(r.Target)
	if err != nil || target != r.Target {
		return nil, fmt.Errorf("%w: invalid signing target", ErrBadRequest)
	}
	if err := a.validate(r); err != nil {
		return nil, err
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid signing key length")
	}
	h := make(http.Header)
	h.Set(header, wire64.EncodeToString(principal))
	h.Set("Renet-Timestamp", strconv.FormatInt(a.Timestamp, 10))
	h.Set("Renet-Client", a.Client)
	if len(a.Nonce) > 0 {
		h.Set("Renet-Nonce", wire64.EncodeToString(a.Nonce))
	}
	if a.IdempotencyKey != "" {
		h.Set("Idempotency-Key", a.IdempotencyKey)
	}
	h.Set("Renet-Signature", wire64.EncodeToString(ed25519.Sign(key, SigningInput(r, a, kind, principal))))
	return h, nil
}

// VerifyDevice uses the device public key from the headers. The caller checks
// enrollment, request time and endpoint authorization separately.
func VerifyDevice(r Request, h http.Header) error { return verifyHeaders(r, h, "device", nil, nil) }

// VerifyNode requires the public key associated with the expected node ID.
func VerifyNode(r Request, h http.Header, id [16]byte, key ed25519.PublicKey) error {
	return verifyHeaders(r, h, "node", id[:], key)
}

func verifyHeaders(r Request, h http.Header, kind string, id []byte, key ed25519.PublicKey) error {
	target, err := CanonicalTarget(r.Target)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	r.Target = target
	fields := make(map[string]string)
	for name, values := range h {
		name = http.CanonicalHeaderKey(name)
		switch name {
		case deviceHeader, nodeHeader, "Renet-Timestamp", "Renet-Client", "Renet-Nonce", "Idempotency-Key", "Renet-Signature":
		default:
			continue
		}
		if _, seen := fields[name]; seen || len(values) != 1 || values[0] == "" || strings.ContainsAny(values[0], ",\r\n") {
			return fmt.Errorf("%w: invalid %s", ErrBadRequest, name)
		}
		fields[name] = values[0]
	}
	timestamp := fields["Renet-Timestamp"]
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || ts < 0 || strconv.FormatInt(ts, 10) != timestamp {
		return fmt.Errorf("%w: invalid timestamp", ErrBadRequest)
	}
	nonce, err := wire64.DecodeString(fields["Renet-Nonce"])
	if err != nil {
		return fmt.Errorf("%w: invalid nonce", ErrBadRequest)
	}
	a := RequestAuth{ts, fields["Renet-Client"], nonce, fields["Idempotency-Key"]}
	if err := a.validate(r); err != nil {
		return err
	}
	var principal []byte
	if kind == "device" {
		principal, err = wire64.DecodeString(fields[deviceHeader])
		if err != nil || len(principal) != ed25519.PublicKeySize || fields[nodeHeader] != "" {
			return fmt.Errorf("%w: invalid device identity", ErrBadRequest)
		}
		key = principal
	} else {
		if fields[nodeHeader] != wire64.EncodeToString(id) || fields[deviceHeader] != "" {
			return fmt.Errorf("%w: unexpected node identity", ErrBadRequest)
		}
		principal = id
	}
	sig, err := wire64.DecodeString(fields["Renet-Signature"])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: invalid signature encoding", ErrBadRequest)
	}
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, SigningInput(r, a, kind, principal), sig) {
		return ErrUnauthenticated
	}
	return nil
}
