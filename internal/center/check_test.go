package center

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"remote-enet/internal/settings"
)

func TestCheckAttemptsEveryEntryInOrder(t *testing.T) {
	requests := make(chan string, 4)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		switch r.URL.Path {
		case "/primary/v1/meta":
			_, _ = io.WriteString(w, validMeta)
		case "/slow/v1/meta":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/mismatch/v1/meta":
			_, _ = io.WriteString(w, strings.Replace(validMeta, "garage", "other", 1))
		case "/unavailable/v1/meta":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			t.Errorf("unexpected request to %s", r.URL)
		}
	}))
	defer server.Close()
	cfg := settings.Center{ID: "garage", API: []string{
		server.URL + "/primary", server.URL + "/slow", server.URL + "/mismatch", server.URL + "/unavailable",
	}}
	got := check(t.Context(), server.Client(), cfg, 300*time.Millisecond)
	want := []EntryResult{
		{Entry: cfg.API[0], Status: Verified},
		{Entry: cfg.API[1], Status: Unreachable},
		{Entry: cfg.API[2], Status: IDMismatch},
		{Entry: cfg.API[3], Status: Unreachable},
	}
	if !slices.Equal(got, want) || len(requests) != len(want) {
		t.Fatalf("results = %v, request count = %d, want %v", got, len(requests), want)
	}
	for _, entry := range cfg.API {
		if path := <-requests; path != strings.TrimPrefix(entry, server.URL)+"/v1/meta" {
			t.Fatalf("out-of-order request: %s for %s", path, entry)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if results := Check(ctx, cfg); len(results) != 0 {
		t.Fatalf("canceled scan returned results: %v", results)
	}
	if results := Check(t.Context(), settings.Center{}); len(results) != 0 {
		t.Fatalf("unconfigured scan returned results: %v", results)
	}
}
