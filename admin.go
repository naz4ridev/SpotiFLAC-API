package main

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"spotiflacapi/internal/c2config"
)

//go:embed webadmin/index.html
var adminIndexHTML []byte

// registerAdminRoutes wires the C2 CRUD API and web UI onto the mux. Access
// control is handled at the reverse proxy (e.g. HTTP basic auth).
func (s *apiServer) registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", s.handleAdminUI)
	mux.HandleFunc("GET /admin/", s.handleAdminUI)
	mux.HandleFunc("GET /admin/c2", s.handleAdminC2List)
	mux.HandleFunc("POST /admin/c2", s.handleAdminC2Create)
	mux.HandleFunc("PUT /admin/c2/{id}", s.handleAdminC2Update)
	mux.HandleFunc("DELETE /admin/c2/{id}", s.handleAdminC2Delete)
	mux.HandleFunc("POST /admin/c2/import", s.handleAdminC2Import)
	mux.HandleFunc("POST /admin/c2/reload", s.handleAdminC2Reload)
	mux.HandleFunc("PUT /admin/settings/{key}", s.handleAdminSetSetting)
}

func (s *apiServer) cfgOrError(w http.ResponseWriter) bool {
	if s.cfg == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{OK: false, Error: "config store not initialized"})
		return false
	}
	return true
}

func (s *apiServer) handleAdminUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(adminIndexHTML)
}

func (s *apiServer) handleAdminC2List(w http.ResponseWriter, _ *http.Request) {
	if !s.cfgOrError(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"endpoints": s.cfg.Endpoints(),
		"settings":  s.cfg.Settings(),
	})
}

func (s *apiServer) handleAdminC2Create(w http.ResponseWriter, r *http.Request) {
	if !s.cfgOrError(w) {
		return
	}
	ep, err := decodeEndpoint(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Error: err.Error()})
		return
	}
	id, err := s.cfg.UpsertEndpoint(ep)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func (s *apiServer) handleAdminC2Update(w http.ResponseWriter, r *http.Request) {
	if !s.cfgOrError(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Error: "invalid id"})
		return
	}
	ep, err := decodeEndpoint(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Error: err.Error()})
		return
	}
	ep.ID = id
	if err := s.cfg.UpdateEndpointByID(ep); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func (s *apiServer) handleAdminC2Delete(w http.ResponseWriter, r *http.Request) {
	if !s.cfgOrError(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Error: "invalid id"})
		return
	}
	if err := s.cfg.DeleteEndpoint(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *apiServer) handleAdminC2Import(w http.ResponseWriter, r *http.Request) {
	if !s.cfgOrError(w) {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Error: "read body: " + err.Error()})
		return
	}
	eps, settings, err := s.cfg.ImportManifest(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "endpoints": eps, "settings": settings})
}

func (s *apiServer) handleAdminC2Reload(w http.ResponseWriter, _ *http.Request) {
	if !s.cfgOrError(w) {
		return
	}
	if err := s.cfg.Reload(); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *apiServer) handleAdminSetSetting(w http.ResponseWriter, r *http.Request) {
	if !s.cfgOrError(w) {
		return
	}
	key := r.PathValue("key")
	if strings.TrimSpace(key) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Error: "missing key"})
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Error: "invalid body"})
		return
	}
	if err := s.cfg.SetSetting(key, body.Value); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func decodeEndpoint(r *http.Request) (c2config.Endpoint, error) {
	var ep c2config.Endpoint
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ep); err != nil {
		return ep, err
	}
	ep.Service = strings.TrimSpace(ep.Service)
	ep.Role = strings.TrimSpace(ep.Role)
	ep.URL = strings.TrimSpace(ep.URL)
	return ep, nil
}

// buildSeedFromEnv assembles the initial store contents from the existing
// environment configuration, so deployments that still ship a .env keep their
// Monochrome instances, credentials, and any C2 overrides on first boot.
func buildSeedFromEnv() c2config.Seed {
	seed := c2config.Seed{Settings: map[string]string{}}

	// Monochrome lists & credentials, mirrored as settings.
	put := func(key, val string) {
		if strings.TrimSpace(val) != "" {
			seed.Settings[key] = val
		}
	}
	put("monochrome.api_instances", strings.Join(splitCSVEnv("MONOCHROME_API_INSTANCES", defaultMonochromeAPIInstances), ","))
	put("monochrome.streaming_instances", strings.Join(splitCSVEnv("MONOCHROME_STREAMING_INSTANCES", defaultMonochromeStreamingInstances), ","))
	put("monochrome.discovery_urls", strings.Join(splitCSVEnv("MONOCHROME_DISCOVERY_URLS", defaultMonochromeDiscoveryURLs), ","))
	put("monochrome.tidal_client_id", envStringDefault("MONOCHROME_TIDAL_CLIENT_ID", defaultMonochromeTidalClientID))
	put("monochrome.tidal_client_secret", envStringDefault("MONOCHROME_TIDAL_CLIENT_SECRET", defaultMonochromeTidalClientSecret))

	// Status source: gist payload that lists which downloader methods are up.
	put("status.source_url", envStringDefault("DOWNLOADER_STATUS_URL",
		"https://gist.githubusercontent.com/afkarxyz/6e57cd362cbd67f889e3a91a76254a5e/raw"))

	// Spotbye supporter API keys for the deezer/tidal/amazon C2 (qobuz needs none).
	put("spotbye.deezer_api_key", strings.TrimSpace(os.Getenv("SPOTBYE_DEEZER_API_KEY")))
	put("spotbye.tidal_api_key", strings.TrimSpace(os.Getenv("SPOTBYE_TIDAL_API_KEY")))
	put("spotbye.amazon_api_key", strings.TrimSpace(os.Getenv("SPOTBYE_AMAZON_API_KEY")))
	put("spotbye.qobuz_search", envStringDefault("SPOTBYE_QOBUZ_SEARCH", "https://qbzmt.spotbye.qzz.io/api/search?q=%s"))

	// Lyrics: Spotify color-lyrics needs the sp_dc cookie (authenticated token).
	put("lyrics.spotify_sp_dc", strings.TrimSpace(os.Getenv("SPOTIFY_SP_DC")))
	put("lyrics.spotify_totp_secret", strings.TrimSpace(os.Getenv("SPOTIFY_TOTP_SECRET")))
	put("lyrics.spotify_totp_version", strings.TrimSpace(os.Getenv("SPOTIFY_TOTP_VERSION")))
	put("lyrics.provider_order", strings.TrimSpace(os.Getenv("LYRICS_PROVIDER_ORDER")))

	return seed
}
