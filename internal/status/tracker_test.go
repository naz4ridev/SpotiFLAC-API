package status

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNormalizeNext(t *testing.T) {
	raw := map[string]string{
		"tidal_a": "down", "tidal_x": "up",
		"qobuz_a": "up", "qobuz_x": "down",
		"apple": "up",
		"deezer_a": "down", "deezer_b": "down",
	}
	got := NormalizeNext(raw)

	if !got["tidal"].Active {
		t.Error("tidal should be active (tidal_x is up)")
	}
	if got["tidal"].Variants["a"] || !got["tidal"].Variants["x"] {
		t.Errorf("tidal variants wrong: %+v", got["tidal"].Variants)
	}
	if !got["apple"].Active || !got["apple"].Variants[""] {
		t.Errorf("apple (bare key) should be active with empty variant: %+v", got["apple"])
	}
	if got["deezer"].Active {
		t.Error("deezer should be inactive (all variants down)")
	}
}

func TestTrackerFetchAndCache(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"tidal_x":"up","qobuz_a":"down","apple":"up"}`))
	}))
	defer srv.Close()

	tr := New(Config{
		HTTPClient:      srv.Client(),
		TTL:             time.Minute,
		StatusSourceURL: func() string { return srv.URL },
	})

	ctx := context.Background()
	if !tr.ServiceActive(ctx, "tidal") {
		t.Error("tidal should be active")
	}
	if tr.ServiceActive(ctx, "qobuz") {
		t.Error("qobuz should be inactive")
	}
	// Unknown service defaults to active so gating never blocks blindly.
	if !tr.ServiceActive(ctx, "amazon") {
		t.Error("unknown service should default active")
	}
	// Second call within TTL must hit cache (no extra HTTP request).
	_ = tr.Get(ctx)
	if hits != 1 {
		t.Errorf("expected 1 upstream fetch (cached after), got %d", hits)
	}
}
