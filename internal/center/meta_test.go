package center

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

const validMeta = `{"center_id":"garage","api_versions":["v1"],"server_time":1795132800,"min_app_version":{"operator":"1.2.3","client":"1.2.4","hub":"1.2.5","future":true},"future":{"a":1}}`

func TestFetchMetaAnonymous(t *testing.T) {
	cases := []struct{ role, version, want string }{
		{"hub", "1.2.3", "hub/1.2.3"},
		{"client", "1.2.3", "client/1.2.3"},
		{"operator", "0.1.0-preview.wave14.3-worktree+001", "operator/0.1.0-preview.wave14.3-worktree+001"},
		{"operator", "dev", "operator/0.0.0-dev"},
		{"client", "", "client/0.0.0-dev"},
		{"client", "v1.2.3", "client/0.0.0-dev"},
		{"client", "01.2.3", "client/0.0.0-dev"},
		{"client", "1.2.3-01", "client/0.0.0-dev"},
		{"client", "1.2.3+private data", "client/0.0.0-dev"},
	}
	headers := make(chan []string, len(cases))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/edge%2Fapi/v1/meta" || r.ContentLength != 0 {
			t.Errorf("unexpected request: %s %s, length %d", r.Method, r.URL, r.ContentLength)
		}
		for name := range r.Header {
			if name != "User-Agent" && name != "Accept-Encoding" && name != "Renet-Client" {
				t.Errorf("unexpected request header: %s", name)
			}
		}
		if r.UserAgent() != "Remote-ENET" {
			t.Errorf("unexpected user agent: %q", r.UserAgent())
		}
		headers <- r.Header.Values("Renet-Client")
		http.SetCookie(w, &http.Cookie{Name: "issued", Value: "ignored"})
		_, _ = io.WriteString(w, validMeta)
	}))
	t.Cleanup(server.Close)
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(base, []*http.Cookie{{Name: "session", Value: "private", Path: "/"}})
	client := server.Client()
	client.Jar = jar
	for _, tc := range cases {
		meta, err := FetchMeta(t.Context(), client, server.URL+"/edge%2Fapi/", tc.role, tc.version)
		if err != nil {
			t.Fatal(err)
		}
		if got := <-headers; !slices.Equal(got, []string{tc.want}) {
			t.Errorf("Renet-Client for %q = %q, want %q", tc.version, got, tc.want)
		}
		if meta.CenterID != "garage" || !slices.Equal(meta.APIVersions, []string{"v1"}) ||
			meta.ServerTime != time.Unix(1795132800, 0).UTC() ||
			meta.MinAppVersion != (MinAppVersion{Operator: "1.2.3", Client: "1.2.4", Hub: "1.2.5"}) {
			t.Fatalf("meta = %+v", meta)
		}
	}
	if cookies := jar.Cookies(base); len(cookies) != 1 || cookies[0].Name != "session" {
		t.Fatalf("caller jar changed: %v", cookies)
	}
}

func TestFetchMetaResponse(t *testing.T) {
	full := validMeta + strings.Repeat(" ", maxMetaBody-len(validMeta))
	for _, tc := range []struct {
		name, body, code, wantErr string
		status                    int
	}{
		{name: "at limit", body: full, status: http.StatusOK},
		{name: "oversized", body: full + " ", status: http.StatusOK, wantErr: "exceeds"},
		{name: "error envelope", body: `{"error":{"code":"unavailable","message":"private content","details":{}}}`, code: "unavailable", status: http.StatusServiceUnavailable},
		{name: "proxy error", body: "private content", status: http.StatusBadGateway},
		{name: "invalid error code type", body: `{"error":{"code":{"private content":1}}}`, status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			_, err := FetchMeta(t.Context(), server.Client(), server.URL, "client", "dev")
			switch {
			case tc.status != http.StatusOK:
				var status *HTTPError
				if !errors.As(err, &status) || status.StatusCode != tc.status || status.Code != tc.code {
					t.Fatalf("got %v (%+v), want HTTP %d, code %q", err, status, tc.status, tc.code)
				}
				if strings.Contains(err.Error(), "private content") {
					t.Fatal("error disclosed response content")
				}
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatal(err)
			}
		})
	}
}

func TestFetchMetaRejectsRedirectAndHTTP(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("followed redirect to a different entry")
	}))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same/v1/meta":
			http.Redirect(w, r, "/redirect-target", http.StatusFound)
		case "/other/v1/meta":
			http.Redirect(w, r, target.URL, http.StatusFound)
		default:
			t.Errorf("unexpected request to %s", r.URL)
		}
	}))
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		t.Error("used caller redirect policy")
		return nil
	}
	for _, prefix := range []string{"/same", "/other"} {
		_, err := FetchMeta(t.Context(), client, server.URL+prefix, "client", "dev")
		var status *HTTPError
		if !errors.As(err, &status) || status.StatusCode != http.StatusFound {
			t.Errorf("%s: expected redirect error, got %v", prefix, err)
		}
	}
	if _, err := FetchMeta(t.Context(), client, "http:"+strings.TrimPrefix(server.URL, "https:"), "client", "dev"); err == nil || !strings.Contains(err.Error(), "invalid HTTPS entry") {
		t.Fatalf("expected HTTPS validation error, got %v", err)
	}
	if _, err := FetchMeta(t.Context(), client, server.URL, "unknown", "1.2.3"); err == nil || !strings.Contains(err.Error(), "requires a client, operator or hub role") {
		t.Fatalf("expected role validation error, got %v", err)
	}
}

func TestMetaRejectsInvalidRequiredFields(t *testing.T) {
	for _, tc := range []struct{ name, old, replacement string }{
		{"missing center ID", `"center_id":"garage",`, ""},
		{"wrong field case", `"center_id"`, `"CENTER_ID"`},
		{"invalid center ID", `"garage"`, `"Garage"`},
		{"long center ID", `"garage"`, fmt.Sprintf("%q", strings.Repeat("a", 33))},
		{"missing API versions", `"api_versions":["v1"],`, ""},
		{"null API versions", `["v1"]`, "null"},
		{"wrong API versions type", `["v1"]`, `"v1"`},
		{"null array element", `["v1"]`, "[null]"},
		{"wrong array element type", `["v1"]`, "[1]"},
		{"missing server time", `"server_time":1795132800,`, ""},
		{"null server time", "1795132800", "null"},
		{"fractional server time", "1795132800", "1795132800.5"},
		{"string server time", "1795132800", `"1795132800"`},
		{"missing minimum version object", `"min_app_version":{"operator":"1.2.3","client":"1.2.4","hub":"1.2.5","future":true},`, ""},
		{"missing minimum version", `"operator":"1.2.3",`, ""},
		{"null minimum version", `"1.2.4"`, "null"},
		{"wrong minimum version type", `"1.2.5"`, "5"},
		{"duplicate center ID", `"center_id":"garage",`, `"center_id":"unknown","center_id":"garage",`},
		{"invalid UTF-8 in unknown field", `"a"`, "\"\xff\""},
		{"trailing JSON", validMeta, validMeta + "{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(validMeta, tc.old, tc.replacement, 1)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			if _, err := FetchMeta(t.Context(), server.Client(), server.URL, "client", "dev"); err == nil {
				t.Fatal("accepted invalid meta")
			}
		})
	}
}
