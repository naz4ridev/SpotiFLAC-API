// Package lyrics fetches song lyrics from the same three providers as
// SpotiFLAC-Next, with synced (LRC) preferred over plain text:
//
//   - Spotify color-lyrics : requires an sp_dc cookie (authenticated token).
//   - Musixmatch           : public token.get -> usertoken -> macro.subtitles.get.
//   - LRCLIB               : public, via the upstream backend client.
//
// The flows mirror SpotiFLAC / SpotiFLAC-Next (the Spotify token flow and TOTP
// secret are taken from the upstream backend source; the Musixmatch public
// app_id flow and LRCLIB are verified live).
package lyrics

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/afkarxyz/SpotiFLAC/backend"
)

// Result is the provider-agnostic lyrics result.
type Result struct {
	Source       string `json:"source"`
	Synced       bool   `json:"synced"`
	LRC          string `json:"lrc,omitempty"`
	Plain        string `json:"plain,omitempty"`
	Instrumental bool   `json:"instrumental"`
}

// Options configures a lyrics lookup.
type Options struct {
	SpotifyID   string
	Track       string
	Artist      string
	Album       string
	ISRC        string
	DurationSec int

	// Spotify provider config.
	SpDc        string // sp_dc cookie; if empty the Spotify provider is skipped
	TotpSecret  string // base32 TOTP secret; defaults to the known Spotify secret
	TotpVersion int    // TOTP version; defaults to the known value

	// Order of providers to try; defaults to spotify, musixmatch, lrclib.
	Order []string

	HTTP *http.Client
}

func (o *Options) httpClient() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// FetchAll tries each configured provider in order and returns the first synced
// result; if none are synced it returns the first plain (unsynced) result.
func FetchAll(ctx context.Context, opts Options) (*Result, error) {
	order := opts.Order
	if len(order) == 0 {
		order = []string{"spotify", "musixmatch", "lrclib"}
	}

	var unsynced *Result
	var lastErr error
	for _, name := range order {
		var (
			res *Result
			err error
		)
		switch strings.ToLower(name) {
		case "spotify":
			if strings.TrimSpace(opts.SpDc) == "" {
				continue // no cookie -> provider unavailable
			}
			res, err = fetchSpotify(ctx, opts)
		case "musixmatch":
			res, err = fetchMusixmatch(ctx, opts)
		case "lrclib":
			res, err = fetchLRCLib(ctx, opts)
		default:
			continue
		}
		if err != nil {
			lastErr = err
			continue
		}
		if res == nil {
			continue
		}
		if res.Synced || res.Instrumental {
			return res, nil
		}
		if unsynced == nil {
			unsynced = res
		}
	}
	if unsynced != nil {
		return unsynced, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("lyrics not found in any source")
}

// --- LRCLIB (via upstream backend client) -----------------------------------

func fetchLRCLib(_ context.Context, opts Options) (*Result, error) {
	c := backend.NewLyricsClient()
	resp, err := c.FetchLyricsWithMetadata(opts.Track, opts.Artist, opts.Album, opts.DurationSec)
	if err != nil || resp == nil || resp.Error || len(resp.Lines) == 0 {
		// Fall back to LRCLIB search.
		resp, err = c.FetchLyricsFromLRCLibSearch(opts.Track, opts.Artist)
		if err != nil || resp == nil || resp.Error || len(resp.Lines) == 0 {
			return nil, fmt.Errorf("lrclib: no lyrics")
		}
	}
	synced := strings.EqualFold(resp.SyncType, "LINE_SYNCED")
	out := &Result{Source: "LRCLIB", Synced: synced, Plain: plainFromLines(resp.Lines)}
	if synced {
		out.LRC = c.ConvertToLRC(resp, opts.Track, opts.Artist)
	}
	out.Instrumental = strings.TrimSpace(out.Plain) == ""
	return out, nil
}

// --- shared helpers ----------------------------------------------------------

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

// lrcTimestamp formats milliseconds as an LRC [mm:ss.xx] tag.
func lrcTimestamp(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	cs := (ms % 1000) / 10
	totalSec := ms / 1000
	return fmt.Sprintf("[%02d:%02d.%02d]", totalSec/60, totalSec%60, cs)
}

// plainFromLRC strips [..] timestamp tags from an LRC body.
func plainFromLRC(lrc string) string {
	var b strings.Builder
	for _, line := range strings.Split(lrc, "\n") {
		txt := line
		for {
			i := strings.IndexByte(txt, '[')
			j := strings.IndexByte(txt, ']')
			if i != 0 || j <= i {
				break
			}
			txt = txt[j+1:]
		}
		txt = strings.TrimSpace(txt)
		if txt != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(txt)
		}
	}
	return b.String()
}
