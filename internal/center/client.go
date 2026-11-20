package center

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"remote-enet/internal/settings"
)

const (
	fallbackAttempts     = 3
	fallbackTotalTimeout = 30 * time.Second
	maxResponseBody      = 256 << 10
	heartbeatPath        = "/v1/node/heartbeat"
)

// ClientOptions binds one role and signing principal to one center.
type ClientOptions struct {
	Center           settings.Center
	Role, AppVersion string
	Device, NodeKey  ed25519.PrivateKey
	Node             [16]byte
	HTTP             *http.Client
	Now              func() time.Time
	Logger           *slog.Logger
}

// Call.Key belongs to the caller across retries and subsequent Do calls.
// Nonce requests disclosure of a code. Wait corresponds to the query's wait_s.
type Call struct {
	Method, Path string
	Query        url.Values
	Body         any
	Key          string
	Nonce        bool
	Wait         time.Duration
}

// Response identifies the entry that answered a signed request.
type Response struct {
	Status   int
	Replayed bool
	Entry    string
}

type entry struct {
	base, origin, prefix string
	gate                 chan struct{}
	status               atomic.Value // Status, initially nil
}

// Client shares verified entries and device time correction across concurrent calls.
type Client struct {
	o                            ClientOptions
	entries                      []*entry
	offset                       atomic.Int64
	attemptTimeout, totalTimeout time.Duration
}

// NewClient starts with no verified entries and performs no network I/O.
func NewClient(o ClientOptions) (*Client, error) {
	cfg := settings.Default()
	cfg.Center = o.Center
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if len(o.Center.API) == 0 {
		return nil, fmt.Errorf("center: no entries")
	}
	switch o.Role {
	case "operator", "client":
		if len(o.Device) != ed25519.PrivateKeySize || len(o.NodeKey) != 0 {
			return nil, fmt.Errorf("center: expected device key")
		}
	case "hub":
		if len(o.NodeKey) != ed25519.PrivateKeySize || len(o.Device) != 0 {
			return nil, fmt.Errorf("center: expected node key")
		}
	default:
		return nil, fmt.Errorf("center: invalid role")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if !semverPattern.MatchString(o.AppVersion) {
		o.AppVersion = devVersion
	}
	httpClient := http.Client{}
	if o.HTTP != nil {
		httpClient = *o.HTTP
	}
	httpClient.Jar = nil
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	o.HTTP = &httpClient
	o.Device = slices.Clone(o.Device)
	o.NodeKey = slices.Clone(o.NodeKey)
	c := &Client{o: o, attemptTimeout: fallbackAttemptTimeout, totalTimeout: fallbackTotalTimeout}
	for _, base := range o.Center.API {
		u, err := url.Parse(base)
		if err != nil {
			return nil, err
		}
		c.entries = append(c.entries, &entry{base: base, origin: u.Scheme + "://" + u.Host, prefix: strings.TrimRight(u.EscapedPath(), "/"), gate: make(chan struct{}, 1)})
	}
	return c, nil
}

// Now uses the same center-time correction as device signatures. Hub time is local.
func (c *Client) Now() time.Time { return c.o.Now().Add(time.Duration(c.offset.Load())) }

// NewIdempotencyKey supplies 128 bits of randomness for one logical operation.
func NewIdempotencyKey() string { return wire64.EncodeToString(random16()) }

func random16() []byte {
	var b [16]byte
	rand.Read(b[:])
	return b[:]
}

func (c *Client) verify(ctx context.Context, e *entry) error {
	if e.status.Load() == Verified {
		return nil
	}
	select {
	case e.gate <- struct{}{}:
		defer func() { <-e.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	switch e.status.Load() {
	case Verified:
		return nil
	case IDMismatch:
		return ErrUnreachable
	}
	meta, err := FetchMeta(ctx, c.o.HTTP, e.base, c.o.Role, c.o.AppVersion)
	if err != nil {
		return err
	}
	if meta.CenterID != c.o.Center.ID {
		e.status.Store(IDMismatch)
		c.o.Logger.ErrorContext(ctx, "Center entry ID mismatch", "entry", e.base)
		return ErrUnreachable
	}
	if c.o.Role != "hub" {
		c.offset.Store(int64(meta.ServerTime.Sub(c.o.Now())))
	}
	e.status.Store(Verified)
	return nil
}

func (c *Client) prepare(call Call) ([]byte, error) {
	if !strings.HasPrefix(call.Path, "/v1/") || strings.ContainsAny(call.Path, "?#") {
		return nil, fmt.Errorf("%w: invalid endpoint path", ErrBadRequest)
	}
	path, err := CanonicalTarget(call.Path)
	if err != nil {
		return nil, err
	}
	if call.Wait < 0 || call.Wait > 25*time.Second {
		return nil, fmt.Errorf("%w: invalid wait", ErrBadRequest)
	}
	id, session := strings.CutPrefix(path, "/v1/sessions/")
	create := call.Method == "POST" && path == "/v1/sessions"
	read := call.Method == "GET" && session && id != "" && !strings.Contains(id, "/")
	if call.Nonce && (c.o.Role != "operator" || !(create || read)) {
		return nil, fmt.Errorf("%w: nonce not applicable", ErrBadRequest)
	}
	if call.Path == heartbeatPath && call.Key != "" {
		return nil, fmt.Errorf("%w: heartbeat cannot have an idempotency key", ErrBadRequest)
	}
	if call.Method == "POST" && call.Path != heartbeatPath && !idempotencyPattern.MatchString(call.Key) {
		return nil, fmt.Errorf("%w: invalid idempotency key", ErrBadRequest)
	}
	if call.Body == nil {
		return nil, nil
	}
	body, err := json.Marshal(call.Body)
	if err != nil {
		return nil, fmt.Errorf("center: encode request: %w", err)
	}
	limit := 16 << 10
	if call.Path == heartbeatPath {
		limit = 512 << 10
	}
	if call.Method == "GET" {
		return nil, fmt.Errorf("%w: GET cannot have a body", ErrBadRequest)
	}
	if len(body) > limit {
		return nil, fmt.Errorf("center: request exceeds %d bytes", limit)
	}
	return body, nil
}

// Do starts at the first eligible entry and preserves the encoded body and key.
func (c *Client) Do(ctx context.Context, call Call, out any) (Response, error) {
	budget, cancel := context.WithTimeout(ctx, c.totalTimeout+call.Wait)
	defer cancel()
	body, err := c.prepare(call)
	if err != nil {
		return Response{}, err
	}
	deadline, _ := budget.Deadline()
	idx := 0
	fixed := false
	var resp Response
	var last error
	heartbeat := call.Method == "POST" && call.Path == heartbeatPath
	for attempt := 1; attempt <= fallbackAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return resp, err
		}
		if budget.Err() != nil {
			break
		}
		for skipped := 0; c.entries[idx].status.Load() == IDMismatch; skipped++ {
			if skipped == len(c.entries) {
				return resp, ErrIDMismatch
			}
			idx = (idx + 1) % len(c.entries)
		}
		e := c.entries[idx]
		single, stop := context.WithTimeout(budget, c.attemptTimeout+call.Wait)
		resp, err = c.attempt(single, e, call, body, out)
		stop()
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return resp, ctx.Err()
		}
		// Probes may fall back, but a sent heartbeat must never replay its seq.
		final := attempt == fallbackAttempts || (heartbeat && resp.Entry != "")
		var api *Error
		if !errors.As(err, &api) || api.Class == ClassRetry {
			if api == nil && !errors.Is(err, ErrUnreachable) {
				return resp, err
			}
			last = err
			if final {
				break
			}
			idx = (idx + 1) % len(c.entries)
			continue
		}
		if final {
			return resp, api
		}
		switch api.Class {
		case ClassWait:
			if api.RetryAfter < 0 || api.RetryAfter >= time.Until(deadline) {
				return resp, api
			}
			select {
			case <-time.After(api.RetryAfter):
			case <-budget.Done():
				if ctx.Err() != nil {
					return resp, ctx.Err()
				}
				return resp, api
			}
		case ClassFix:
			if fixed || resp.Entry == "" {
				return resp, api
			}
			if api.Code == "clock_skew" {
				ts, ok := api.ServerTime()
				if !ok || c.o.Role == "hub" {
					return resp, api
				}
				c.offset.Store(int64(ts.Sub(c.o.Now())))
			} else if !call.Nonce {
				return resp, api
			}
			fixed = true
		default:
			return resp, api
		}
		last = api
	}
	// The final allowed probe may exclude the last remaining entry.
	if !slices.ContainsFunc(c.entries, func(e *entry) bool { return e.status.Load() != IDMismatch }) {
		return resp, ErrIDMismatch
	}
	var api *Error
	if errors.As(last, &api) && api.Class != ClassRetry {
		return resp, api
	}
	if last == nil {
		return resp, ErrUnreachable
	}
	return resp, fmt.Errorf("%w: %w", ErrUnreachable, last)
}

func (c *Client) attempt(ctx context.Context, e *entry, call Call, body []byte, out any) (Response, error) {
	raw := e.prefix + call.Path
	if q := call.Query.Encode(); q != "" {
		raw += "?" + q
	}
	target, err := CanonicalTarget(raw)
	if err != nil {
		return Response{}, err
	}
	if err := c.verify(ctx, e); err != nil {
		return Response{}, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	r := Request{CenterID: c.o.Center.ID, Method: call.Method, Target: target, Body: body}
	auth := RequestAuth{Timestamp: c.Now().Unix(), Client: c.o.Role + "/" + c.o.AppVersion, IdempotencyKey: call.Key}
	if call.Nonce {
		auth.Nonce = random16()
	}
	req, err := http.NewRequestWithContext(ctx, call.Method, e.origin+target, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.GetBody = nil // Do owns retries, including fresh nonces and signatures.
	if c.o.Role == "hub" {
		req.Header, err = SignNode(r, auth, c.o.NodeKey, c.o.Node)
	} else {
		req.Header, err = SignDevice(r, auth, c.o.Device)
	}
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("User-Agent", "Remote-ENET")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.o.HTTP.Do(req)
	if err != nil {
		return Response{Entry: e.base}, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer res.Body.Close()
	result := Response{Status: res.StatusCode, Entry: e.base, Replayed: res.Header.Get("Idempotency-Replayed") == "true"}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBody+1))
	if err != nil {
		return result, fmt.Errorf("%w: read response: %w", ErrUnreachable, err)
	}
	if len(data) > maxResponseBody {
		return result, fmt.Errorf("%w: response exceeds %d bytes", ErrProtocol, maxResponseBody)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return result, responseError(res.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return result, ErrProtocol
		}
	}
	return result, nil
}
