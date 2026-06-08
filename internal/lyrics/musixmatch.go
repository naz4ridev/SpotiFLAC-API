package lyrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Musixmatch flow (public, verified live): token.get with the web-desktop app id
// yields a usertoken, then macro.subtitles.get returns a synced LRC subtitle
// and/or plain lyrics. No HMAC signature is required for this app id.

const musixmatchAppID = "web-desktop-app-v1.0"

func musixmatchToken(ctx context.Context, client *http.Client) (string, error) {
	u := fmt.Sprintf("https://apic.musixmatch.com/ws/1.1/token.get?app_id=%s&format=json", musixmatchAppID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", spotifyUA)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var data struct {
		Message struct {
			Body struct {
				UserToken string `json:"user_token"`
			} `json:"body"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	if data.Message.Body.UserToken == "" {
		return "", fmt.Errorf("musixmatch: empty user token")
	}
	return data.Message.Body.UserToken, nil
}

func fetchMusixmatch(ctx context.Context, opts Options) (*Result, error) {
	client := opts.httpClient()
	token, err := musixmatchToken(ctx, client)
	if err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("format", "json")
	q.Set("namespace", "lyrics_richsynched")
	q.Set("subtitle_format", "lrc")
	q.Set("app_id", musixmatchAppID)
	q.Set("usertoken", token)
	q.Set("q_track", opts.Track)
	q.Set("q_artist", opts.Artist)
	if opts.Album != "" {
		q.Set("q_album", opts.Album)
	}
	if opts.DurationSec > 0 {
		q.Set("q_duration", strconv.Itoa(opts.DurationSec))
	}
	if opts.ISRC != "" {
		q.Set("track_isrc", opts.ISRC)
	}

	u := "https://apic.musixmatch.com/ws/1.1/macro.subtitles.get?" + q.Encode()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", spotifyUA)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	subtitle, plain, instrumental := parseMusixmatchMacro(body)
	if subtitle != "" {
		return &Result{Source: "Musixmatch", Synced: true, LRC: subtitle, Plain: plainFromLRC(subtitle)}, nil
	}
	if instrumental {
		return &Result{Source: "Musixmatch", Instrumental: true}, nil
	}
	if strings.TrimSpace(plain) != "" {
		return &Result{Source: "Musixmatch", Synced: false, Plain: plain}, nil
	}
	return nil, fmt.Errorf("musixmatch: no lyrics")
}

// parseMusixmatchMacro extracts the synced subtitle (LRC), plain lyrics, and the
// instrumental flag from a macro.subtitles.get response.
func parseMusixmatchMacro(body []byte) (subtitle, plain string, instrumental bool) {
	var root struct {
		Message struct {
			Body struct {
				MacroCalls map[string]json.RawMessage `json:"macro_calls"`
			} `json:"body"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return "", "", false
	}
	calls := root.Message.Body.MacroCalls

	// track.subtitles.get -> subtitle_list[0].subtitle.subtitle_body (LRC)
	if raw, ok := calls["track.subtitles.get"]; ok {
		var sub struct {
			Message struct {
				Body struct {
					SubtitleList []struct {
						Subtitle struct {
							SubtitleBody string `json:"subtitle_body"`
						} `json:"subtitle"`
					} `json:"subtitle_list"`
				} `json:"body"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &sub) == nil && len(sub.Message.Body.SubtitleList) > 0 {
			subtitle = sub.Message.Body.SubtitleList[0].Subtitle.SubtitleBody
		}
	}

	// track.lyrics.get -> lyrics.lyrics_body / instrumental
	if raw, ok := calls["track.lyrics.get"]; ok {
		var lyr struct {
			Message struct {
				Body struct {
					Lyrics struct {
						LyricsBody   string `json:"lyrics_body"`
						Instrumental int    `json:"instrumental"`
					} `json:"lyrics"`
				} `json:"body"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &lyr) == nil {
			plain = lyr.Message.Body.Lyrics.LyricsBody
			instrumental = lyr.Message.Body.Lyrics.Instrumental == 1
		}
	}
	return subtitle, plain, instrumental
}
