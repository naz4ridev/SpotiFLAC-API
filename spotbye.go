package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	case "apple":
		return "am" // single host (no a..e variant pool), uses mode= not quality=
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

		resp, verr := s.spotbyePoolGet(ctx, service, prefix, id)
		if verr != nil {
			attempts = append(attempts, attempt{Service: service, Error: verr.Error()})
			continue
		}

		outputPath := filepath.Join(outputDir, buildMonochromeFilename(meta))
		if err := s.spotbyeDownload(ctx, resp, outputPath, meta); err != nil {
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

// spotbyePoolGet tries each live variant host's /api/dl until one returns a
// usable response. Apple uses a single host (am.{domain}) and a "mode" param;
// the others use {prefix}-{variant} hosts and a "quality" param.
func (s *apiServer) spotbyePoolGet(ctx context.Context, service, prefix, id string) (*spotbyeDLResponse, error) {
	// Base domain and /api/dl path are overridable via the config store so the
	// pool can be repointed if SpotiFLAC-Next moves it, without a Go redeploy.
	domain, dlPath := spotbyeBaseDomain, "/api/dl"
	if s.cfg != nil {
		domain = s.cfg.Setting("spotbye.base_domain", domain)
		dlPath = s.cfg.Setting("spotbye.dl_path", dlPath)
	}

	// Build the request body (apple: mode=alac; others: quality=<q>).
	var body []byte
	if service == "apple" {
		body, _ = json.Marshal(map[string]string{"id": id, "mode": "alac"})
	} else {
		body, _ = json.Marshal(map[string]string{"id": id, "quality": spotbyeQuality(service)})
	}

	// Apple has no a..e pool: a single host. Others rotate the live variants.
	var hosts []string
	if service == "apple" {
		hosts = []string{fmt.Sprintf("https://%s.%s%s", prefix, domain, dlPath)}
	} else {
		for _, v := range s.spotbyeVariants(ctx, service) {
			hosts = append(hosts, fmt.Sprintf("https://%s-%s.%s%s", prefix, v, domain, dlPath))
		}
	}

	var lastErr error
	for _, host := range hosts {
		resp, err := s.spotbyeAPIDL(ctx, host, body)
		if err != nil {
			lastErr = err
			continue
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no active variants")
	}
	return nil, fmt.Errorf("spotbye %s: pool exhausted: %w", service, lastErr)
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
	URL     string `json:"url"`
	Quality string `json:"quality"`
	Mode    string `json:"mode"`
	Key     string `json:"key"`
}

// spotbyeAPIDL POSTs a prepared body to a pool host's /api/dl and returns the
// parsed response (must contain a url).
func (s *apiServer) spotbyeAPIDL(ctx context.Context, endpoint string, body []byte) (*spotbyeDLResponse, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", defaultMonochromeUserAgent)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var data spotbyeDLResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if !strings.HasPrefix(data.URL, "http") && !strings.HasPrefix(data.URL, "MANIFEST:") {
		return nil, fmt.Errorf("no usable url in response")
	}
	return &data, nil
}

// spotbyeDownload writes the track to outputPath from a pool response:
//   - "MANIFEST:<b64>"  -> Tidal DASH, decoded + muxed by ffmpeg (inline-dash:)
//   - url + key         -> Amazon CENC mp4: download, mp4ff-decrypt, transcode
//   - plain url         -> direct stream, muxed by ffmpeg
func (s *apiServer) spotbyeDownload(ctx context.Context, resp *spotbyeDLResponse, outputPath string, meta trackMetadata) error {
	if strings.HasPrefix(resp.URL, "MANIFEST:") {
		return downloadMonochromeTrack(ctx, "inline-dash:"+strings.TrimPrefix(resp.URL, "MANIFEST:"), outputPath, meta)
	}
	if strings.TrimSpace(resp.Key) != "" {
		return s.spotbyeDownloadEncrypted(ctx, resp.URL, resp.Key, outputPath, meta)
	}
	return downloadMonochromeTrack(ctx, resp.URL, outputPath, meta)
}

// spotbyeDownloadEncrypted downloads an Amazon-style CENC mp4, decrypts it with
// the KID:KEY, then transcodes to FLAC (with metadata) via ffmpeg.
func (s *apiServer) spotbyeDownloadEncrypted(ctx context.Context, mediaURL, keySpec, outputPath string, meta trackMetadata) error {
	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	encPath := filepath.Join(dir, "enc.mp4")
	decPath := filepath.Join(dir, "dec.mp4")
	defer func() { _ = os.Remove(encPath); _ = os.Remove(decPath) }()

	if err := downloadFileTo(ctx, s.httpClient, mediaURL, encPath); err != nil {
		return fmt.Errorf("fetch encrypted mp4: %w", err)
	}
	if err := decryptAmazonMP4(keySpec, encPath, decPath); err != nil {
		return fmt.Errorf("decrypt: %w", err)
	}
	// Transcode the decrypted mp4 (ALAC/AAC) to FLAC, tagging with metadata.
	return downloadMonochromeTrack(ctx, decPath, outputPath, meta)
}

// downloadFileTo streams a URL to a local file.
func downloadFileTo(ctx context.Context, client *http.Client, rawURL, dest string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	req.Header.Set("User-Agent", defaultMonochromeUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
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
	case "apple":
		return s.resolveAppleID(ctx, meta)
	default:
		return "", fmt.Errorf("unsupported service: %s", service)
	}
}

var appleSongIDRe = regexp.MustCompile(`[?&]i=(\d+)|/song/(?:[^/]+/)?(\d+)`)

// resolveAppleID finds the Apple Music song id: first via odesli (the appleMusic
// link's i=/song id), then via the public iTunes Search API matched on title.
func (s *apiServer) resolveAppleID(ctx context.Context, meta trackMetadata) (string, error) {
	// 1) odesli appleMusic link.
	odesli := fmt.Sprintf("https://api.song.link/v1-alpha.1/links?url=https://open.spotify.com/track/%s&userCountry=US", url.QueryEscape(meta.SpotifyID))
	var od struct {
		LinksByPlatform map[string]struct {
			URL string `json:"url"`
		} `json:"linksByPlatform"`
		Entities map[string]struct {
			ID           string `json:"id"`
			APIProvider  string `json:"apiProvider"`
		} `json:"entitiesByUniqueId"`
	}
	if err := s.spotbyeGetJSON(ctx, odesli, &od); err == nil {
		if l, ok := od.LinksByPlatform["appleMusic"]; ok && l.URL != "" {
			if m := appleSongIDRe.FindStringSubmatch(l.URL); m != nil {
				if m[1] != "" {
					return m[1], nil
				}
				if m[2] != "" {
					return m[2], nil
				}
			}
		}
		for _, e := range od.Entities {
			if (e.APIProvider == "appleMusic" || e.APIProvider == "itunes") && e.ID != "" {
				return e.ID, nil
			}
		}
	}

	// 2) iTunes Search fallback (match the title; no auth needed).
	if id := s.resolveAppleViaITunes(ctx, meta); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("apple: could not resolve song id (not on odesli/iTunes)")
}

func (s *apiServer) resolveAppleViaITunes(ctx context.Context, meta trackMetadata) string {
	term := strings.TrimSpace(meta.Name + " " + firstArtist(meta.Artists))
	u := fmt.Sprintf("https://itunes.apple.com/search?term=%s&entity=song&limit=10&country=US", url.QueryEscape(term))
	var res struct {
		Results []struct {
			TrackID    int64  `json:"trackId"`
			TrackName  string `json:"trackName"`
			ArtistName string `json:"artistName"`
		} `json:"results"`
	}
	if err := s.spotbyeGetJSON(ctx, u, &res); err != nil {
		return ""
	}
	wantTitle := strings.ToLower(strings.TrimSpace(meta.Name))
	for _, r := range res.Results {
		if r.TrackID != 0 && strings.Contains(strings.ToLower(r.TrackName), wantTitle) {
			return fmt.Sprintf("%d", r.TrackID)
		}
	}
	if len(res.Results) > 0 && res.Results[0].TrackID != 0 {
		return fmt.Sprintf("%d", res.Results[0].TrackID)
	}
	return ""
}

// spotbyeGetJSON does a GET and decodes JSON (shared by the id resolvers).
func (s *apiServer) spotbyeGetJSON(ctx context.Context, endpoint string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.Header.Set("User-Agent", defaultMonochromeUserAgent)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
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
