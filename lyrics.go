package main

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"spotiflacapi/internal/lyrics"
)

// lyricsResponse is the API shape for GET /v1/lyrics.
type lyricsResponse struct {
	OK           bool   `json:"ok"`
	SpotifyID    string `json:"spotify_id"`
	Track        string `json:"track,omitempty"`
	Artist       string `json:"artist,omitempty"`
	Source       string `json:"source,omitempty"`
	Synced       bool   `json:"synced"`
	Instrumental bool   `json:"instrumental"`
	LRC          string `json:"lrc,omitempty"`
	PlainLyrics  string `json:"plain_lyrics,omitempty"`
}

// handleLyrics fetches lyrics using the same three providers as SpotiFLAC-Next:
// Spotify color-lyrics (needs an sp_dc cookie), Musixmatch, and LRCLIB, synced
// preferred. Query: spotify_url|spotify_id; format=json|lrc|text.
func (s *apiServer) handleLyrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{OK: false, Error: "method not allowed"})
		return
	}

	input := strings.TrimSpace(r.URL.Query().Get("spotify_url"))
	if input == "" {
		input = strings.TrimSpace(r.URL.Query().Get("spotify_id"))
	}
	if input == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Error: "spotify_url or spotify_id is required"})
		return
	}

	spotifyID, err := extractSpotifyTrackID(input)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Error: err.Error()})
		return
	}
	spotifyURL := spotifyTrackURLBase + spotifyID

	meta, err := fetchTrackMetadata(spotifyURL)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorResponse{OK: false, Error: "failed to fetch metadata: " + err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	durationSec := 0
	if ms, derr := fetchExpectedDurationMs(ctx, spotifyURL); derr == nil && ms > 0 {
		durationSec = int(ms / 1000)
	}

	opts := lyrics.Options{
		SpotifyID:   spotifyID,
		Track:       meta.Name,
		Artist:      meta.Artists,
		Album:       meta.AlbumName,
		ISRC:        meta.ISRC,
		DurationSec: durationSec,
		SpDc:        s.lyricsSetting("lyrics.spotify_sp_dc", "SPOTIFY_SP_DC"),
		TotpSecret:  s.lyricsSetting("lyrics.spotify_totp_secret", "SPOTIFY_TOTP_SECRET"),
		Order:       splitCSVSetting(s.lyricsSetting("lyrics.provider_order", "LYRICS_PROVIDER_ORDER"), nil),
		HTTP:        s.httpClient,
	}
	if v := s.lyricsSetting("lyrics.spotify_totp_version", "SPOTIFY_TOTP_VERSION"); v != "" {
		if n, perr := strconv.Atoi(strings.TrimSpace(v)); perr == nil {
			opts.TotpVersion = n
		}
	}

	res, err := lyrics.FetchAll(ctx, opts)
	if err != nil || res == nil {
		writeJSON(w, http.StatusNotFound, lyricsResponse{
			OK: false, SpotifyID: spotifyID, Track: meta.Name, Artist: meta.Artists,
		})
		return
	}

	switch strings.ToLower(r.URL.Query().Get("format")) {
	case "lrc":
		body := res.LRC
		if body == "" {
			body = res.Plain
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(body))
	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(res.Plain))
	default:
		writeJSON(w, http.StatusOK, lyricsResponse{
			OK:           true,
			SpotifyID:    spotifyID,
			Track:        meta.Name,
			Artist:       meta.Artists,
			Source:       res.Source,
			Synced:       res.Synced,
			Instrumental: res.Instrumental,
			LRC:          res.LRC,
			PlainLyrics:  res.Plain,
		})
	}
}

// lyricsSetting prefers the config store, then the environment.
func (s *apiServer) lyricsSetting(settingKey, envName string) string {
	if s.cfg != nil {
		if v := s.cfg.Setting(settingKey, ""); v != "" {
			return v
		}
	}
	return strings.TrimSpace(os.Getenv(envName))
}

func splitCSVSetting(raw string, fallback []string) []string {
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}
