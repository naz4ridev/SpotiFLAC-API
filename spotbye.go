package main

import (
	"bytes"
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

// spotbye.go implements the SpotiFLAC-Next "Shared/Community" download pool — the
// REAL, live workflow (reverse-engineered + verified against the running C2):
//
//   POST https://{prefix}-{variant}.spotbye.qzz.io/api/dl
//        {"id": "<service track id>", "quality": "<q>"}
//   -> {"url": "<direct FLAC url>"}                 (qobuz, deezer)
//   -> {"url": "MANIFEST:<base64 DASH mpd>"}        (tidal)
//   -> {"url": "<encrypted mp4 url>", "key": ...}   (amazon — needs mp4 decrypt)
//
// prefix: tidal=tdl, qobuz=qbz, amazon=amz, deezer=dzr. The variant letters come
// from the downloader-status payload (tidal_a..e = Shared pool, tidal_x =
// Community); we try every "up" variant in order. Track ids are resolved like
// SpotiFLAC-Next: qobuz via the qbzmt metadata search (ISRC/hi-res match), deezer
// via the public Deezer ISRC lookup, tidal/amazon via odesli.

const spotbyeBaseDomain = "spotbye.qzz.io"

var (
	tidalIDRe  = regexp.MustCompile(`(?:track/)(\d+)`)
	amazonIDRe = regexp.MustCompile(`(?:tracks/)([A-Za-z0-9]+)`)
)

func spotbyePrefix(service string) string {
	switch service {
	case "tidal":
		return "tdl"
	case "qobuz":
		return "qbz"
	case "amazon":
		return "amz"
	case "deezer":
		return "dzr"
	default:
		return ""
	}
}

// spotbyeQuality returns the default quality token accepted by /api/dl per service.
func spotbyeQuality(service string) string {
	switch service {
	case "qobuz", "tidal":
		return "24"
	case "deezer":
		return "320"
	case "amazon":
		return "16"
	default:
		return "16"
	}
}

// resolveWithSpotbye downloads via the SpotiFLAC-Next Shared/Community pool.
func (s *apiServer) resolveWithSpotbye(ctx context.Context, meta trackMetadata, serviceOrder []string, outputDir string) (string, string, []attempt, error) {
	attempts := make([]attempt, 0, len(serviceOrder))

	for _, service := range serviceOrder {
		service = strings.ToLower(service)
		prefix := spotbyePrefix(service)
		if prefix == "" {
			attempts = append(attempts, attempt{Service: service, Error: "spotbye: unsupported service"})
			continue
		}

		id, err := s.spotbyeServiceID(ctx, service, meta)
		if err != nil {
			attempts = append(attempts, attempt{Service: service, Error: err.Error()})
			continue
		}

		mediaURL, verr := s.spotbyePoolResolve(ctx, service, prefix, id)
		if verr != nil {
			attempts = append(attempts, attempt{Service: service, Error: verr.Error()})
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

// spotbyePoolResolve tries each "up" variant host's /api/dl until one returns an
// ffmpeg-ingestible media URL (direct stream or a Tidal DASH manifest).
func (s *apiServer) spotbyePoolResolve(ctx context.Context, service, prefix, id string) (string, error) {
	variants := s.spotbyeVariants(ctx, service)
	quality := spotbyeQuality(service)
	// Base domain and /api/dl path are overridable via the config store so the
	// pool can be repointed if SpotiFLAC-Next moves it, without a Go redeploy.
	domain, dlPath := spotbyeBaseDomain, "/api/dl"
	if s.cfg != nil {
		domain = s.cfg.Setting("spotbye.base_domain", domain)
		dlPath = s.cfg.Setting("spotbye.dl_path", dlPath)
	}
	var lastErr error
	for _, v := range variants {
		host := fmt.Sprintf("https://%s-%s.%s%s", prefix, v, domain, dlPath)
		media, err := s.spotbyeAPIDL(ctx, host, id, quality)
		if err != nil {
			lastErr = err
			continue
		}
		return media, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no active variants")
	}
	return "", fmt.Errorf("spotbye %s: pool exhausted: %w", service, lastErr)
}

// spotbyeVariants returns the variant suffixes to try (status "up" ones first;
// falls back to the full a..e,x pool when status is unavailable).
func (s *apiServer) spotbyeVariants(ctx context.Context, service string) []string {
	if s.status != nil {
		if v := s.status.ActiveVariants(ctx, service); len(v) > 0 {
			return v
		}
	}
	return []string{"a", "b", "c", "d", "e", "x"}
}

type spotbyeDLResponse struct {
	URL     string          `json:"url"`
	Quality string          `json:"quality"`
	Key     json.RawMessage `json:"key"`
	Success *bool           `json:"success"`
	Error   json.RawMessage `json:"error"`
}

// spotbyeAPIDL POSTs to a pool host's /api/dl and returns an ffmpeg-ready URL.
func (s *apiServer) spotbyeAPIDL(ctx context.Context, endpoint, id, quality string) (string, error) {
	body, _ := json.Marshal(map[string]string{"id": id, "quality": quality})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", defaultMonochromeUserAgent)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var data spotbyeDLResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&data); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	if data.URL == "" {
		return "", fmt.Errorf("no url in response")
	}
	if len(data.Key) > 0 && string(data.Key) != "null" {
		// Amazon-style encrypted MP4 + key: needs mp4ff decryption, which the
		// upstream backend handles — defer so the auto chain falls back to it.
		return "", fmt.Errorf("encrypted stream (key present); deferring to upstream")
	}
	if strings.HasPrefix(data.URL, "MANIFEST:") {
		// Tidal DASH manifest (base64). downloadMonochromeTrack understands the
		// "inline-dash:" prefix and feeds the decoded .mpd to ffmpeg.
		return "inline-dash:" + strings.TrimPrefix(data.URL, "MANIFEST:"), nil
	}
	if !strings.HasPrefix(data.URL, "http") {
		return "", fmt.Errorf("unexpected url form")
	}
	return data.URL, nil
}

// --- per-service track-id resolution ----------------------------------------

func (s *apiServer) spotbyeServiceID(ctx context.Context, service string, meta trackMetadata) (string, error) {
	switch service {
	case "qobuz":
		return s.resolveQobuzID(ctx, meta)
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

type qobuzTrack struct {
	ID              int64  `json:"id"`
	ISRC            string `json:"isrc"`
	Title           string `json:"title"`
	Hires           bool   `json:"hires"`
	MaximumBitDepth int    `json:"maximum_bit_depth"`
}

// resolveQobuzID finds the best Qobuz track id via the qbzmt metadata search
// (exact ISRC match preferred, else the highest-quality title match).
func (s *apiServer) resolveQobuzID(ctx context.Context, meta trackMetadata) (string, error) {
	searchTmpl := "https://qbzmt.spotbye.qzz.io/api/search?q=%s"
	if s.cfg != nil {
		searchTmpl = s.cfg.Setting("spotbye.qobuz_search", searchTmpl)
	}
	query := strings.TrimSpace(meta.Name + " " + firstArtist(meta.Artists))
	searchURL := strings.ReplaceAll(searchTmpl, "%s", url.QueryEscape(query))

	var sr struct {
		Data struct {
			Tracks struct {
				Items []qobuzTrack `json:"items"`
			} `json:"tracks"`
		} `json:"data"`
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	req.Header.Set("User-Agent", defaultMonochromeUserAgent)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("qobuz search: %w", err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&sr); err != nil {
		return "", fmt.Errorf("qobuz search decode: %w", err)
	}
	items := sr.Data.Tracks.Items
	if len(items) == 0 {
		return "", fmt.Errorf("qobuz: no search results for %q", query)
	}
	id := pickQobuzTrack(items, meta.ISRC)
	if id == 0 {
		return "", fmt.Errorf("qobuz: no suitable track")
	}
	return fmt.Sprintf("%d", id), nil
}

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
			return best[i].Hires
		}
		return best[i].MaximumBitDepth > best[j].MaximumBitDepth
	})
	return best[0].ID
}

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

func (s *apiServer) resolveDeezerID(ctx context.Context, isrc string) (int64, error) {
	api := fmt.Sprintf("https://api.deezer.com/track/isrc:%s", url.PathEscape(isrc))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("deezer isrc lookup: %w", err)
	}
	defer resp.Body.Close()
	var payload struct {
		ID    int64 `json:"id"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return 0, fmt.Errorf("deezer isrc decode: %w", err)
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
