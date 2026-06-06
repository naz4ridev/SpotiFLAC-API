package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/afkarxyz/SpotiFLAC/backend"
)

// spotbye.go implements the "spotbye" download engine: the private SpotiFLAC-Next
// C2 endpoints. Endpoint URLs and API keys are read from the C2 config store
// (seeded from the extracted manifest, editable at /admin/), and the active
// variant is chosen via the status tracker.
//
// Contracts (verified live, 2026-06):
//   qobuz  : NO key needed.
//            1) resolve qobuz id: GET qbzmt.spotbye.qzz.io/api/search?q={name artist}
//               -> data.tracks.items[] each {id, isrc, title, hires, maximum_bit_depth}
//            2) GET qbz.spotbye.qzz.io/api/download-music?track_id={id}
//               -> {"success":true,"data":{"url":"<flac stream>"}}
//   deezer : GET deezer.anandserver.cfd/api/track/{deezer_id}  (needs X-API-Key)
//   tidal  : tidal.anandserver.cfd + flacdownloader.com        (needs X-API-Key)
//   amazon : amz.squid.wtf                                      (needs X-API-Key)
//
// The deezer/tidal/amazon keys are supporter-issued (self-serve generation was
// retired; see antra.anandserver.cfd) and are NOT in the binary, so they live in
// the config store (settings spotbye.<service>_api_key, or spotbye.api_key) and
// are sent as the X-API-Key header. qobuz needs none.

var (
	tidalIDRe  = regexp.MustCompile(`(?:track/)(\d+)`)
	amazonIDRe = regexp.MustCompile(`(?:tracks/)([A-Za-z0-9]+)`)
)

// resolveWithSpotbye tries each requested service via its spotbye C2 endpoint.
func (s *apiServer) resolveWithSpotbye(ctx context.Context, meta trackMetadata, serviceOrder []string, outputDir string) (string, string, []attempt, error) {
	attempts := make([]attempt, 0, len(serviceOrder))
	if s.cfg == nil {
		return "", "", attempts, fmt.Errorf("spotbye engine requires the C2 config store")
	}

	for _, service := range serviceOrder {
		if s.status != nil && !s.status.ServiceActive(ctx, service) {
			attempts = append(attempts, attempt{Service: service, Error: "skipped: no active variant in status"})
			continue
		}

		mediaURL, err := s.spotbyeResolveMedia(ctx, service, meta)
		if err != nil {
			attempts = append(attempts, attempt{Service: service, Error: err.Error()})
			continue
		}

		outputPath := filepath.Join(outputDir, buildMonochromeFilename(meta))
		if err := downloadMonochromeTrack(ctx, mediaURL, outputPath, meta); err != nil {
			attempts = append(attempts, attempt{Service: service, Error: "download failed: " + err.Error()})
			continue
		}
		if !isValidDownloadedFile(outputPath) {
			attempts = append(attempts, attempt{Service: service, Error: "downloaded file invalid"})
			continue
		}
		attempts = append(attempts, attempt{Service: service})
		return outputPath, service, attempts, nil
	}

	return "", "", attempts, fmt.Errorf("spotbye failed in all services: %s", strings.Join(serviceOrder, " -> "))
}

// spotbyeResolveMedia resolves an ffmpeg-ingestible media URL for a track.
func (s *apiServer) spotbyeResolveMedia(ctx context.Context, service string, meta trackMetadata) (string, error) {
	switch strings.ToLower(service) {
	case "qobuz":
		return s.spotbyeQobuz(ctx, meta)
	case "deezer":
		return s.spotbyeByID(ctx, "deezer", meta)
	case "tidal":
		return s.spotbyeByID(ctx, "tidal", meta)
	case "amazon":
		return s.spotbyeByID(ctx, "amazon", meta)
	case "apple":
		return "", fmt.Errorf("spotbye apple: id resolution not implemented yet")
	default:
		return "", fmt.Errorf("unsupported spotbye service: %s", service)
	}
}

// --- qobuz (verified, no key) -----------------------------------------------

type qobuzTrack struct {
	ID              int64  `json:"id"`
	ISRC            string `json:"isrc"`
	Title           string `json:"title"`
	Hires           bool   `json:"hires"`
	MaximumBitDepth int    `json:"maximum_bit_depth"`
}

// spotbyeQobuz resolves the best qobuz track id (matching ISRC, else highest
// quality title match) and returns its FLAC stream URL.
func (s *apiServer) spotbyeQobuz(ctx context.Context, meta trackMetadata) (string, error) {
	searchTmpl := s.spotbyeSetting("spotbye.qobuz_search", "https://qbzmt.spotbye.qzz.io/api/search?q=%s")
	query := strings.TrimSpace(meta.Name + " " + firstArtist(meta.Artists))
	searchURL := strings.ReplaceAll(searchTmpl, "%s", url.QueryEscape(query))

	var sr struct {
		Data struct {
			Tracks struct {
				Items []qobuzTrack `json:"items"`
			} `json:"tracks"`
		} `json:"data"`
	}
	if err := s.spotbyeGetJSON(ctx, searchURL, "", &sr); err != nil {
		return "", fmt.Errorf("qobuz search: %w", err)
	}
	items := sr.Data.Tracks.Items
	if len(items) == 0 {
		return "", fmt.Errorf("qobuz: no search results for %q", query)
	}

	id := pickQobuzTrack(items, meta.ISRC)
	if id == 0 {
		return "", fmt.Errorf("qobuz: no suitable track match")
	}

	dlBase := firstEnabledURL(s.cfg.EnabledURLs("qobuz", "download"),
		"https://qbz.spotbye.qzz.io/api/download-music?track_id=")
	dlURL := applyIDTemplate(dlBase, fmt.Sprintf("%d", id))

	media, err := s.spotbyeFetchMedia(ctx, dlURL, "")
	if err != nil {
		return "", fmt.Errorf("qobuz download: %w", err)
	}
	return media, nil
}

// pickQobuzTrack chooses the best track: exact ISRC match first, otherwise the
// highest-quality candidate (hires, then bit depth).
func pickQobuzTrack(items []qobuzTrack, isrc string) int64 {
	isrc = strings.ToUpper(strings.TrimSpace(isrc))
	if isrc != "" {
		for _, t := range items {
			if strings.ToUpper(strings.TrimSpace(t.ISRC)) == isrc {
				return t.ID
			}
		}
	}
	best := append([]qobuzTrack(nil), items...)
	sort.SliceStable(best, func(i, j int) bool {
		if best[i].Hires != best[j].Hires {
			return best[i].Hires // hires first
		}
		return best[i].MaximumBitDepth > best[j].MaximumBitDepth
	})
	return best[0].ID
}

// --- id-template services (deezer / tidal / amazon, X-API-Key) ---------------

// spotbyeByID resolves the service id, then tries each configured endpoint with
// the service's API key until one yields a media URL.
func (s *apiServer) spotbyeByID(ctx context.Context, service string, meta trackMetadata) (string, error) {
	id, err := s.spotbyeServiceID(ctx, service, meta)
	if err != nil {
		return "", err
	}
	apiKey := s.spotbyeAPIKey(service)
	endpoints := s.spotbyeEndpoints(service)
	if len(endpoints) == 0 {
		return "", fmt.Errorf("spotbye %s: no endpoint configured", service)
	}
	var lastErr error
	for _, tmpl := range endpoints {
		media, err := s.spotbyeFetchMedia(ctx, applyIDTemplate(tmpl, id), apiKey)
		if err != nil {
			lastErr = err
			continue
		}
		return media, nil
	}
	if apiKey == "" {
		return "", fmt.Errorf("spotbye %s: all endpoints failed (no X-API-Key set; deezer/tidal/amazon need a supporter key in setting spotbye.%s_api_key): %v", service, service, lastErr)
	}
	return "", fmt.Errorf("spotbye %s: all endpoints failed: %v", service, lastErr)
}

// spotbyeServiceID resolves the service-specific track id via odesli / Deezer.
func (s *apiServer) spotbyeServiceID(ctx context.Context, service string, meta trackMetadata) (string, error) {
	switch service {
	case "deezer":
		if strings.TrimSpace(meta.ISRC) == "" {
			return "", fmt.Errorf("deezer: missing ISRC")
		}
		dz, err := s.resolveDeezerID(ctx, meta.ISRC)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d", dz), nil
	case "tidal":
		if u := s.resolveSongLinkURLs(meta.SpotifyID); u != nil {
			if m := tidalIDRe.FindStringSubmatch(u.TidalURL); len(m) == 2 {
				return m[1], nil
			}
		}
		return "", fmt.Errorf("tidal: could not resolve id via odesli")
	case "amazon":
		if u := s.resolveSongLinkURLs(meta.SpotifyID); u != nil {
			if m := amazonIDRe.FindStringSubmatch(u.AmazonURL); len(m) == 2 {
				return m[1], nil
			}
		}
		return "", fmt.Errorf("amazon: could not resolve id via odesli")
	default:
		return "", fmt.Errorf("unsupported service: %s", service)
	}
}

// spotbyeEndpoints returns ordered C2 endpoint templates for a service (store
// first, then live-host-first defaults).
func (s *apiServer) spotbyeEndpoints(service string) []string {
	var out []string
	for _, role := range []string{"download", "download_alt", "download_fallback"} {
		for _, u := range s.cfg.EnabledURLs(service, role) {
			if strings.TrimSpace(u) != "" {
				out = append(out, u)
			}
		}
	}
	if len(out) == 0 {
		switch service {
		case "deezer":
			out = []string{"https://deezer.anandserver.cfd/api/track/%s"}
		case "amazon":
			out = []string{"https://amz.squid.wtf/api/track/%s", "https://amz.spotbye.qzz.io/api/track/%s"}
		case "tidal":
			out = []string{"https://tidal.anandserver.cfd/api/track/%s", "https://flacdownloader.com/flac/download?id=%s"}
		}
	}
	return out
}

// --- helpers ----------------------------------------------------------------

// spotbyeSetting reads a store setting with a fallback.
func (s *apiServer) spotbyeSetting(key, fallback string) string {
	if s.cfg != nil {
		return s.cfg.Setting(key, fallback)
	}
	return fallback
}

// spotbyeAPIKey returns the supporter API key for a service: a per-service
// setting, else a shared one.
func (s *apiServer) spotbyeAPIKey(service string) string {
	if s.cfg == nil {
		return ""
	}
	if k := s.cfg.Setting("spotbye."+service+"_api_key", ""); k != "" {
		return k
	}
	return s.cfg.Setting("spotbye.api_key", "")
}

// resolveSongLinkURLs fetches the odesli URL map for a Spotify track.
func (s *apiServer) resolveSongLinkURLs(spotifyID string) *backend.SongLinkURLs {
	if strings.TrimSpace(spotifyID) == "" {
		return nil
	}
	urls, err := backend.NewSongLinkClient().GetAllURLsFromSpotify(spotifyID, "US")
	if err != nil {
		return nil
	}
	return urls
}

// resolveDeezerID looks up a Deezer numeric track id from an ISRC.
func (s *apiServer) resolveDeezerID(ctx context.Context, isrc string) (int64, error) {
	api := fmt.Sprintf("https://api.deezer.com/track/isrc:%s", url.PathEscape(isrc))
	var payload struct {
		ID    int64 `json:"id"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := s.spotbyeGetJSON(ctx, api, "", &payload); err != nil {
		return 0, fmt.Errorf("deezer isrc lookup: %w", err)
	}
	if payload.ID == 0 {
		msg := "not found on deezer"
		if payload.Error != nil && payload.Error.Message != "" {
			msg = payload.Error.Message
		}
		return 0, fmt.Errorf("deezer isrc %s: %s", isrc, msg)
	}
	return payload.ID, nil
}

// spotbyeGetJSON performs a GET (with optional X-API-Key) and decodes JSON.
func (s *apiServer) spotbyeGetJSON(ctx context.Context, endpoint, apiKey string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.Header.Set("User-Agent", defaultMonochromeUserAgent)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("returned %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// spotbyeFetchMedia calls a C2 endpoint and returns a media URL ffmpeg can read:
// either the endpoint itself (direct stream) or a JSON media-URL pointer.
func (s *apiServer) spotbyeFetchMedia(ctx context.Context, endpoint, apiKey string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.Header.Set("User-Agent", defaultMonochromeUserAgent)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("c2 request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("c2 returned %d", resp.StatusCode)
	}

	ctype := resp.Header.Get("Content-Type")
	if strings.Contains(ctype, "audio") || strings.Contains(ctype, "flac") || strings.Contains(ctype, "octet-stream") {
		return endpoint, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("c2 read: %w", err)
	}
	if u := extractMediaURL(body); u != "" {
		return u, nil
	}
	if len(body) > 4 && string(body[:4]) == "fLaC" {
		return endpoint, nil
	}
	return "", fmt.Errorf("c2 response had no media url")
}

// mediaURLFields are the JSON keys SpotiFLAC-Next uses for a downloadable URL.
var mediaURLFields = []string{"url", "downloadUrl", "download_url", "streamUrl", "stream", "link", "file", "path"}

// extractMediaURL pulls a media URL out of a C2 JSON response (top level or
// nested under "data").
func extractMediaURL(body []byte) string {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return ""
	}
	if u := pickURL(top); u != "" {
		return u
	}
	if data, ok := top["data"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(data, &nested) == nil {
			if u := pickURL(nested); u != "" {
				return u
			}
		}
	}
	return ""
}

func pickURL(m map[string]json.RawMessage) string {
	for _, f := range mediaURLFields {
		raw, ok := m[f]
		if !ok {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) == nil && strings.HasPrefix(v, "http") {
			return v
		}
	}
	return ""
}

// applyIDTemplate substitutes the track id into an endpoint template.
func applyIDTemplate(endpoint, id string) string {
	if strings.Contains(endpoint, "%d") || strings.Contains(endpoint, "%s") {
		endpoint = strings.ReplaceAll(endpoint, "%d", id)
		endpoint = strings.ReplaceAll(endpoint, "%s", id)
		return endpoint
	}
	if strings.HasSuffix(endpoint, "track_id=") || strings.HasSuffix(endpoint, "id=") || strings.HasSuffix(endpoint, "/") {
		return endpoint + id
	}
	return endpoint
}

// firstEnabledURL returns the first URL from the list, or the fallback if empty.
func firstEnabledURL(urls []string, fallback string) string {
	for _, u := range urls {
		if strings.TrimSpace(u) != "" {
			return u
		}
	}
	return fallback
}
