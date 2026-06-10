package c2config

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "c2.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSeedAndReadAccessors(t *testing.T) {
	s := newTestStore(t)
	if !s.IsEmpty() {
		t.Fatal("fresh store should be empty")
	}
	seeded, err := s.SeedIfEmpty(Seed{
		Endpoints: []Endpoint{
			{Service: "qobuz", Role: "download", URL: "https://qbz.example", Enabled: true, Priority: 10},
			{Service: "qobuz", Role: "download", Variant: "x", URL: "https://qbzalt.example", Enabled: false},
		},
		Settings: map[string]string{"monochrome.api_instances": "https://a,https://b"},
	})
	if err != nil || !seeded {
		t.Fatalf("SeedIfEmpty: seeded=%v err=%v", seeded, err)
	}

	// Seeding again must be a no-op (never clobber operator edits).
	seeded2, err := s.SeedIfEmpty(Seed{Settings: map[string]string{"monochrome.api_instances": "OVERWRITE"}})
	if err != nil || seeded2 {
		t.Fatalf("second SeedIfEmpty should no-op: seeded=%v err=%v", seeded2, err)
	}
	if got := s.Setting("monochrome.api_instances", ""); got != "https://a,https://b" {
		t.Fatalf("setting clobbered: %q", got)
	}

	if got := s.SettingCSV("monochrome.api_instances", nil); len(got) != 2 || got[0] != "https://a" {
		t.Fatalf("SettingCSV = %v", got)
	}

	// Only the enabled endpoint should come back.
	urls := s.EnabledURLs("qobuz", "download")
	if len(urls) != 1 || urls[0] != "https://qbz.example" {
		t.Fatalf("EnabledURLs = %v", urls)
	}
}

func TestUpsertUpdateDelete(t *testing.T) {
	s := newTestStore(t)
	id, err := s.UpsertEndpoint(Endpoint{Service: "tidal", Role: "download", URL: "https://t1", Enabled: true})
	if err != nil {
		t.Fatalf("UpsertEndpoint: %v", err)
	}

	// Upsert on the same (service, role, variant) updates rather than duplicates.
	if _, err := s.UpsertEndpoint(Endpoint{Service: "tidal", Role: "download", URL: "https://t2", Enabled: true}); err != nil {
		t.Fatalf("UpsertEndpoint update: %v", err)
	}
	if eps := s.Endpoints(); len(eps) != 1 {
		t.Fatalf("expected 1 endpoint after upsert, got %d", len(eps))
	}
	if urls := s.EnabledURLs("tidal", "download"); len(urls) != 1 || urls[0] != "https://t2" {
		t.Fatalf("update not applied: %v", urls)
	}

	if err := s.UpdateEndpointByID(Endpoint{ID: id, Service: "tidal", Role: "download", URL: "https://t3", Enabled: false}); err != nil {
		t.Fatalf("UpdateEndpointByID: %v", err)
	}
	if urls := s.EnabledURLs("tidal", "download"); len(urls) != 0 {
		t.Fatalf("expected disabled endpoint to be excluded, got %v", urls)
	}

	if err := s.DeleteEndpoint(id); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if eps := s.Endpoints(); len(eps) != 0 {
		t.Fatalf("expected 0 endpoints after delete, got %d", len(eps))
	}
	if err := s.DeleteEndpoint(id); err == nil {
		t.Fatal("deleting missing endpoint should error")
	}
}

func TestImportManifest(t *testing.T) {
	s := newTestStore(t)
	raw := []byte(`{
	  "endpoints": {
	    "qobuz.download": {"service":"qobuz","role":"download","host":"qbz.spotbye.qzz.io","example_url":"https://qbz.spotbye.qzz.io/api/download-music?track_id"},
	    "_status.status": {"service":"_status","role":"status","host":"status.spotbye.qzz.io","example_url":"https://status.spotbye.qzz.io/status.json"}
	  },
	  "public_providers": {"lyrics.lrclib_get":"https://lrclib.net/api/get"}
	}`)
	eps, settings, err := s.ImportManifest(raw)
	if err != nil {
		t.Fatalf("ImportManifest: %v", err)
	}
	if eps != 2 || settings != 2 { // lrclib_get + status.source_url
		t.Fatalf("import counts: endpoints=%d settings=%d", eps, settings)
	}
	if got := s.Setting("status.source_url", ""); got != "https://status.spotbye.qzz.io/status.json" {
		t.Fatalf("status.source_url = %q", got)
	}
	if got := s.Setting("endpoint.lyrics.lrclib_get", ""); got != "https://lrclib.net/api/get" {
		t.Fatalf("lrclib setting = %q", got)
	}
	if urls := s.EnabledURLs("qobuz", "download"); len(urls) != 1 {
		t.Fatalf("qobuz endpoint not imported: %v", urls)
	}
}

func TestImportManifestReplacesEndpoints(t *testing.T) {
	s := newTestStore(t)

	// A pre-existing / manually-added endpoint and an operator API key.
	if _, err := s.UpsertEndpoint(Endpoint{Service: "stale", Role: "download", URL: "https://old", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("spotbye.deezer_api_key", "SECRET"); err != nil {
		t.Fatal(err)
	}

	raw := []byte(`{"endpoints":{"qobuz.download":{"service":"qobuz","role":"download","host":"h","example_url":"https://new"}},"public_providers":{}}`)
	if _, _, err := s.ImportManifest(raw); err != nil {
		t.Fatalf("ImportManifest: %v", err)
	}

	// The stale endpoint must be gone (replaced, not accumulated).
	if urls := s.EnabledURLs("stale", "download"); len(urls) != 0 {
		t.Fatalf("stale endpoint should have been discarded, got %v", urls)
	}
	if eps := s.Endpoints(); len(eps) != 1 || eps[0].Service != "qobuz" {
		t.Fatalf("expected only the new qobuz endpoint, got %+v", eps)
	}
	// Operator settings must survive an import.
	if got := s.Setting("spotbye.deezer_api_key", ""); got != "SECRET" {
		t.Fatalf("operator API key was clobbered by import: %q", got)
	}
}
