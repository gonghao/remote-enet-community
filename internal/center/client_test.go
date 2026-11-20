package center

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/settings"
)

func testClient(t *testing.T, role string, h http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewTLSServer(h)
	t.Cleanup(server.Close)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	o := ClientOptions{Center: settings.Center{ID: "garage", API: []string{server.URL + "/a", server.URL + "/b"}}, Role: role, HTTP: server.Client(), Now: func() time.Time { return time.Unix(100, 0) }}
	if role == "hub" {
		o.NodeKey = key
		o.Node = [16]byte{1}
	} else {
		o.Device = key
	}
	c, err := NewClient(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientFallbackReplay(t *testing.T) {
	for _, path := range []string{"/v1/sessions", "/v1/redeem"} {
		t.Run(path, func(t *testing.T) {
			key := NewIdempotencyKey()
			role := "client"
			if path == "/v1/sessions" {
				role = "operator"
			}
			var firstNonce string
			var paths []string
			records := map[string]int{}
			c := testClient(t, role, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if strings.HasSuffix(r.URL.Path, "/meta") {
					fmt.Fprint(w, validMeta)
					return
				}
				body, _ := io.ReadAll(r.Body)
				req := Request{"garage", "POST", r.RequestURI, body}
				if err := VerifyDevice(req, r.Header); err != nil {
					t.Error(err)
				}
				if string(body) != `{"value":1}` || r.Header.Get("Idempotency-Key") != key {
					t.Error("body or key changed")
				}
				nonce := r.Header.Get("Renet-Nonce")
				if (nonce != "") != (role == "operator") || (nonce != "" && nonce == firstNonce) {
					t.Error("unexpected nonce")
				}
				firstNonce = nonce
				if records[key] == 0 {
					records[key]++
					w.WriteHeader(502)
					fmt.Fprint(w, "lost response")
					return
				}
				w.Header().Set("Idempotency-Replayed", "true")
				fmt.Fprint(w, `{"id":123}`)
			})
			call := Call{Path: path, Method: "POST", Key: key, Body: map[string]int{"value": 1}, Nonce: role == "operator"}
			for i := 0; i < 2; i++ {
				var out struct {
					ID int `json:"id"`
				}
				res, err := c.Do(t.Context(), call, &out)
				if err != nil || !res.Replayed || out.ID != 123 {
					t.Fatalf("response %+v, out %+v, err %v", res, out, err)
				}
			}
			want := []string{"/a/v1/meta", "/a" + path, "/b/v1/meta", "/b" + path, "/a" + path}
			if records[key] != 1 || !reflect.DeepEqual(paths, want) {
				t.Fatalf("records %v, paths %v", records, paths)
			}
		})
	}
}

func TestClientClassActions(t *testing.T) {
	for _, tc := range []struct {
		code, role           string
		status, count, retry int
		success              bool
	}{
		{"rate_limited", "operator", 429, 2, 1, true},
		{"quota_releasing", "operator", 409, 1, 99, false},
		{"session_ended", "operator", 410, 1, 0, false},
		{"clock_skew", "operator", 401, 2, 0, false},
		{"clock_skew", "hub", 401, 1, 0, false},
		{"nonce_reused", "operator", 409, 2, 0, true},
		{"unavailable", "operator", 503, 3, 0, false},
	} {
		t.Run(tc.code+"_"+tc.role, func(t *testing.T) {
			var requests int
			var nonce string
			c := testClient(t, tc.role, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/meta") {
					fmt.Fprint(w, validMeta)
					return
				}
				requests++
				if !strings.HasPrefix(r.URL.Path, "/a/") && tc.code != "unavailable" {
					t.Error("changed entry")
				}
				ts := "1795132800"
				req := Request{"garage", r.Method, r.RequestURI, nil}
				if tc.role == "hub" {
					ts = "100"
					key := ed25519.NewKeyFromSeed(make([]byte, 32))
					if err := VerifyNode(req, r.Header, [16]byte{1}, key.Public().(ed25519.PublicKey)); err != nil {
						t.Error(err)
					}
				} else if err := VerifyDevice(req, r.Header); err != nil {
					t.Error(err)
				}
				if tc.code == "clock_skew" && requests == 2 {
					ts = "200"
				}
				if got := r.Header.Get("Renet-Timestamp"); got != ts {
					t.Errorf("timestamp %s, want %s", got, ts)
				}
				n := r.Header.Get("Renet-Nonce")
				if tc.code == "nonce_reused" {
					b, _ := wire64.DecodeString(n)
					if len(b) != 16 || n == nonce {
						t.Error("expected fresh nonce")
					}
				} else if n != "" {
					t.Error("unexpected nonce")
				}
				nonce = n
				if tc.success && requests == 2 {
					w.WriteHeader(204)
					return
				}
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"error":{"code":%q,"retry_after_s":%d,"details":{"server_time":200,"end_reason":"device_revoked","state":"ended"}}}`, tc.code, tc.retry)
			})
			call := Call{Method: "GET", Path: "/v1/sessions/id", Nonce: tc.code == "nonce_reused"}
			if tc.role == "hub" {
				call = Call{Method: "POST", Path: heartbeatPath}
			}
			start := time.Now()
			_, err := c.Do(t.Context(), call, nil)
			if requests != tc.count || (err == nil) != tc.success {
				t.Fatalf("%d requests, error %v", requests, err)
			}
			if tc.success && time.Since(start) < time.Duration(tc.retry)*time.Second {
				t.Fatal("retry was early")
			}
			if err != nil {
				var e *Error
				if !errors.As(err, &e) || e.Code != tc.code || e.Status != tc.status || e.State() != "ended" || e.EndReason() != "device_revoked" || e.RetryAfter != time.Duration(tc.retry)*time.Second {
					t.Fatal(err)
				}
				if errors.Is(err, ErrUnreachable) != (tc.code == "unavailable") {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestClientIDMismatch(t *testing.T) {
	var probes int
	c := testClient(t, "hub", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a/v1/meta":
			probes++
			fmt.Fprint(w, strings.Replace(validMeta, "garage", "other", 1))
		case "/b/v1/meta":
			fmt.Fprint(w, validMeta)
		case "/b/v1/node/activate":
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected request %s", r.URL)
		}
	})
	key := NewIdempotencyKey()
	for range 2 {
		if _, err := c.Do(t.Context(), Call{Path: "/v1/node/activate", Method: "POST", Key: key}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if probes != 1 {
		t.Fatal(probes)
	}
}

func TestClientDeadlines(t *testing.T) {
	var hits atomic.Int32
	c := testClient(t, "client", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/meta") {
			fmt.Fprint(w, validMeta)
			return
		}
		hits.Add(1)
		if r.URL.Query().Get("wait_s") == "1" {
			select {
			case <-time.After(160 * time.Millisecond):
				w.WriteHeader(204)
			case <-r.Context().Done():
			}
			return
		}
		<-r.Context().Done()
	})
	c.attemptTimeout = 100 * time.Millisecond
	c.totalTimeout = 125 * time.Millisecond
	call := Call{Path: "/v1/sessions/id", Method: "GET", Wait: time.Second, Query: url.Values{"wait_s": {"1"}, "since_version": {"1"}}}
	if _, err := c.Do(t.Context(), call, nil); err != nil {
		t.Fatal(err)
	}
	hits.Store(0)
	c.attemptTimeout = 5 * time.Second
	call.Wait = 0
	call.Query = nil
	_, err := c.Do(t.Context(), call, nil)
	if !errors.Is(err, ErrUnreachable) || !errors.Is(err, context.DeadlineExceeded) || hits.Load() != 1 {
		t.Fatalf("%d hits, err %v", hits.Load(), err)
	}
	call.Nonce = true
	if _, err := c.Do(t.Context(), call, nil); !errors.Is(err, ErrBadRequest) || hits.Load() != 1 {
		t.Fatal("client requested nonce")
	}
}

func TestClientTimeoutAfterCommit(t *testing.T) {
	key := NewIdempotencyKey()
	var hits atomic.Int32
	c := testClient(t, "client", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/meta") {
			fmt.Fprint(w, validMeta)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"salt":"same"}` || r.Header.Get("Idempotency-Key") != key {
			t.Error("replay changed operation")
		}
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusCreated)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		w.WriteHeader(http.StatusCreated)
	})
	c.attemptTimeout = 150 * time.Millisecond
	res, err := c.Do(t.Context(), Call{Method: "POST", Path: "/v1/redeem", Key: key, Body: map[string]string{"salt": "same"}}, nil)
	if err != nil || hits.Load() != 2 || res.Status != 201 || !res.Replayed || !strings.HasSuffix(res.Entry, "/b") {
		t.Fatalf("response %+v, err %v", res, err)
	}
}

func TestNodeClockSkewOnActivation(t *testing.T) {
	var hits atomic.Int32
	c := testClient(t, "hub", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/meta") {
			fmt.Fprint(w, validMeta)
			return
		}
		hits.Add(1)
		if r.Header.Get("Renet-Timestamp") != "100" {
			t.Error("adjusted node timestamp")
		}
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"code":"clock_skew","details":{"server_time":250}}}`)
	})
	_, err := c.Do(t.Context(), Call{Method: "POST", Path: "/v1/node/activate", Key: NewIdempotencyKey()}, nil)
	var e *Error
	if !errors.As(err, &e) || e.Code != "clock_skew" || hits.Load() != 1 {
		t.Fatalf("error %v, hits %d", err, hits.Load())
	}
}

func TestHeartbeatFallsBackOnlyBeforeSending(t *testing.T) {
	var paths []string
	c := testClient(t, "hub", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/a/v1/meta":
			w.WriteHeader(502)
		case "/b/v1/meta":
			fmt.Fprint(w, validMeta)
		case "/b/v1/node/heartbeat":
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":{"code":"unavailable"}}`)
		default:
			t.Error(r.URL.Path)
		}
	})
	_, err := c.Do(t.Context(), Call{Method: "POST", Path: heartbeatPath}, nil)
	if !errors.Is(err, ErrUnreachable) || !reflect.DeepEqual(paths, []string{"/a/v1/meta", "/b/v1/meta", "/b/v1/node/heartbeat"}) {
		t.Fatalf("paths %v, err %v", paths, err)
	}
}

func TestClientRejectsInvalidCallBeforeProbe(t *testing.T) {
	c := testClient(t, "operator", func(http.ResponseWriter, *http.Request) { t.Error("unexpected network request") })
	for _, call := range []Call{
		{Method: "GET", Path: "/v1/sessions/%2"},
		{Method: "POST", Path: "/v1/sessions"},
		{Method: "GET", Path: "/v1/nodes", Nonce: true},
		{Method: "GET", Path: "/v1/sessions/%2e%2e", Nonce: true},
		{Method: "GET", Path: "/v1/sessions/id", Wait: 26 * time.Second},
		{Method: "POST", Path: "/v1/sessions", Key: NewIdempotencyKey(), Body: strings.Repeat("x", 16<<10)},
	} {
		if _, err := c.Do(t.Context(), call, nil); err == nil {
			t.Errorf("accepted call to %s", call.Path)
		}
	}
}

func TestWaitWithoutUsableDelay(t *testing.T) {
	c := testClient(t, "client", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/meta") {
			fmt.Fprint(w, validMeta)
			return
		}
		w.WriteHeader(409)
		fmt.Fprint(w, `{"error":{"code":"session_not_ready"}}`)
		if !strings.HasPrefix(r.URL.Path, "/a/") {
			t.Error("wait moved to another entry")
		}
	})
	_, err := c.Do(t.Context(), Call{Method: "GET", Path: "/v1/sessions/id"}, nil)
	var e *Error
	if !errors.As(err, &e) || e.Code != "session_not_ready" || e.Class != ClassWait || e.RetryAfter != -1 {
		t.Fatalf("err %v", err)
	}
}
