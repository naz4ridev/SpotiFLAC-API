package main

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/afkarxyz/SpotiFLAC/backend"
)

// lyricsResponse is the API shape for GET /v1/lyrics. It mirrors what
// SpotiFLAC-Next produces: a source label, whether the lyrics are time-synced,
// an LRC blob for synced lyrics, and the plain text fallback.
type lyricsResponse struct {
	OK           bool                `json:"ok"`
	SpotifyID    string              `json:"spotify_id"`
	Track        string              `json:"track,omitempty"`
	Artist       string              `json:"artist,omitempty"`
	Source       string              `json:"source,omitempty"`
	SyncType     string              `json:"sync_type,omitempty"`
	Synced       bool                `json:"synced"`
	Instrumental bool                `json:"instrumental"`
	LRC          string              `json:"lrc,omitempty"`
	PlainLyrics  string              `json:"plain_lyrics,omitempty"`
	Lines        []backend.LyricsLine `json:"lines,omitempty"`
}

// handleLyrics fetches lyrics for a Spotify track using the same multi-source
// chain as SpotiFLAC-Next (LRCLib -> Musixmatch -> Spotify color-lyrics), which
// is implemented by the upstream backend.LyricsClient.FetchLyricsAllSources.
//
// Query params: spotify_url or spotify_id (one required); format=json|lrc|text.
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

	// Duration (seconds) helps LRCLib disambiguate; best-effort.
	durationSec := 0
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if ms, derr := fetchExpectedDurationMs(ctx, spotifyURL); derr == nil && ms > 0 {
		durationSec = int(ms / 1000)
	}

	client := backend.NewLyricsClient()
	resp, source, err := client.FetchLyricsAllSources(meta.SpotifyID, meta.Name, meta.Artists, meta.AlbumName, durationSec)
	if err != nil || resp == nil || resp.Error || len(resp.Lines) == 0 {
		writeJSON(w, http.StatusNotFound, lyricsResponse{
			OK: false, SpotifyID: spotifyID, Track: meta.Name, Artist: meta.Artists, Source: source,
		})
		return
	}

	synced := strings.EqualFold(resp.SyncType, "LINE_SYNCED")
	out := lyricsResponse{
		OK:        true,
		SpotifyID: spotifyID,
		Track:     meta.Name,
		Artist:    meta.Artists,
		Source:    source,
		SyncType:  resp.SyncType,
		Synced:    synced,
	}
	plain := plainFromLines(resp.Lines)
	out.PlainLyrics = plain
	out.Instrumental = strings.TrimSpace(plain) == ""
	if synced {
		out.LRC = client.ConvertToLRC(resp, meta.Name, meta.Artists)
	}

	switch strings.ToLower(r.URL.Query().Get("format")) {
	case "lrc":
		body := out.LRC
		if body == "" {
			body = plain
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(body))
	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(plain))
	default:
		out.Lines = resp.Lines
		writeJSON(w, http.StatusOK, out)
	}
}

// plainFromLines joins synced/unsynced lyric lines into plain text.
func plainFromLines(lines []backend.LyricsLine) string {
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l.Words)
	}
	return b.String()
}
