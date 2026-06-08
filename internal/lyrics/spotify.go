package lyrics

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// Spotify color-lyrics flow, mirroring SpotiFLAC-Next:
//   sp_dc cookie -> open.spotify.com/api/token (TOTP) -> authenticated access token
//   -> clienttoken.spotify.com/v1/clienttoken -> client token
//   -> spclient.wg.spotify.com/color-lyrics/v2/track/{id} (Bearer + Client-Token).
// color-lyrics requires an authenticated (non-anonymous) token, hence sp_dc.

const (
	// Default Spotify TOTP secret/version (from the upstream backend source).
	// These rotate occasionally; override via Options.TotpSecret/TotpVersion.
	defaultSpotifyTOTPSecret  = "GM3TMMJTGYZTQNZVGM4DINJZHA4TGOBYGMZTCMRTGEYDSMJRHE4TEOBUG4YTCMRUGQ4DQOJUGQYTAMRRGA2TCMJSHE3TCMBY"
	defaultSpotifyTOTPVersion = 61
	spotifyUA                 = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
)

var appServerConfigRe = regexp.MustCompile(`<script id="appServerConfig" type="text/plain">([^<]+)</script>`)

type spotifyClient struct {
	http          *http.Client
	spDc          string
	totpSecret    string
	totpVersion   int
	accessToken   string
	clientID      string
	clientToken   string
	clientVersion string
	deviceID      string
	cookies       map[string]string
}

func generateTOTP(secret string, now time.Time) (string, error) {
	key, err := otp.NewKeyFromURL(fmt.Sprintf("otpauth://totp/secret?secret=%s", secret))
	if err != nil {
		return "", err
	}
	return totp.GenerateCode(key.Secret(), now)
}

// addCookies attaches sp_dc plus any accumulated session cookies.
func (c *spotifyClient) addCookies(req *http.Request) {
	if c.spDc != "" {
		req.AddCookie(&http.Cookie{Name: "sp_dc", Value: c.spDc})
	}
	for name, value := range c.cookies {
		if name == "sp_dc" {
			continue
		}
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
}

func (c *spotifyClient) absorbCookies(resp *http.Response) {
	for _, ck := range resp.Cookies() {
		if ck.Name == "sp_t" {
			c.deviceID = ck.Value
		}
		c.cookies[ck.Name] = ck.Value
	}
}

func (c *spotifyClient) getSessionInfo(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://open.spotify.com", nil)
	req.Header.Set("User-Agent", spotifyUA)
	c.addCookies(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if m := appServerConfigRe.FindSubmatch(body); len(m) > 1 {
		if decoded, err := base64.StdEncoding.DecodeString(string(m[1])); err == nil {
			var cfg map[string]any
			if json.Unmarshal(decoded, &cfg) == nil {
				if v, ok := cfg["clientVersion"].(string); ok {
					c.clientVersion = v
				}
			}
		}
	}
	c.absorbCookies(resp)
	return nil
}

func (c *spotifyClient) getAccessToken(ctx context.Context) error {
	code, err := generateTOTP(c.totpSecret, time.Now())
	if err != nil {
		return fmt.Errorf("totp: %w", err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://open.spotify.com/api/token", nil)
	q := req.URL.Query()
	q.Add("reason", "transport")
	q.Add("productType", "web-player")
	q.Add("totp", code)
	q.Add("totpVer", strconv.Itoa(c.totpVersion))
	q.Add("totpServer", code)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("User-Agent", spotifyUA)
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	c.addCookies(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("api/token HTTP %d", resp.StatusCode)
	}
	var data struct {
		AccessToken string `json:"accessToken"`
		ClientID    string `json:"clientId"`
		IsAnonymous bool   `json:"isAnonymous"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return err
	}
	if data.AccessToken == "" {
		return fmt.Errorf("api/token returned no access token")
	}
	if data.IsAnonymous {
		return fmt.Errorf("token is anonymous (invalid or expired sp_dc cookie)")
	}
	c.accessToken = data.AccessToken
	c.clientID = data.ClientID
	c.absorbCookies(resp)
	return nil
}

func (c *spotifyClient) getClientToken(ctx context.Context) error {
	payload := map[string]any{
		"client_data": map[string]any{
			"client_version": c.clientVersion,
			"client_id":      c.clientID,
			"js_sdk_data": map[string]any{
				"device_brand": "unknown",
				"device_model": "unknown",
				"os":           "windows",
				"os_version":   "NT 10.0",
				"device_id":    c.deviceID,
				"device_type":  "computer",
			},
		},
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://clienttoken.spotify.com/v1/clienttoken", strings.NewReader(string(body)))
	req.Header.Set("Authority", "clienttoken.spotify.com")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", spotifyUA)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("clienttoken HTTP %d", resp.StatusCode)
	}
	var data struct {
		ResponseType string `json:"response_type"`
		GrantedToken struct {
			Token string `json:"token"`
		} `json:"granted_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return err
	}
	c.clientToken = data.GrantedToken.Token
	return nil
}

type colorLyricsResponse struct {
	Lyrics struct {
		SyncType string `json:"syncType"`
		Provider string `json:"provider"`
		Lines    []struct {
			StartTimeMs string `json:"startTimeMs"`
			Words       string `json:"words"`
			EndTimeMs   string `json:"endTimeMs"`
		} `json:"lines"`
	} `json:"lyrics"`
}

func (c *spotifyClient) colorLyrics(ctx context.Context, spotifyID string) (*Result, error) {
	url := fmt.Sprintf("https://spclient.wg.spotify.com/color-lyrics/v2/track/%s?format=json&market=from_token", spotifyID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", spotifyUA)
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("App-Platform", "WebPlayer")
	req.Header.Set("Spotify-App-Version", c.clientVersion)
	if c.clientToken != "" {
		req.Header.Set("Client-Token", c.clientToken)
	}
	req.Header.Set("Accept", "application/json")
	c.addCookies(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, fmt.Errorf("no lyrics for track")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("color-lyrics HTTP %d", resp.StatusCode)
	}
	var data colorLyricsResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	if len(data.Lyrics.Lines) == 0 {
		return nil, fmt.Errorf("empty lyrics")
	}

	synced := strings.EqualFold(data.Lyrics.SyncType, "LINE_SYNCED")
	var lrc, plain strings.Builder
	for _, ln := range data.Lyrics.Lines {
		if plain.Len() > 0 {
			plain.WriteByte('\n')
		}
		plain.WriteString(ln.Words)
		if synced {
			ms, _ := strconv.ParseInt(strings.TrimSpace(ln.StartTimeMs), 10, 64)
			lrc.WriteString(lrcTimestamp(ms))
			lrc.WriteString(ln.Words)
			lrc.WriteByte('\n')
		}
	}
	res := &Result{Source: "Spotify", Synced: synced, Plain: plain.String()}
	if synced {
		res.LRC = lrc.String()
	}
	res.Instrumental = strings.TrimSpace(res.Plain) == ""
	return res, nil
}

func fetchSpotify(ctx context.Context, opts Options) (*Result, error) {
	if strings.TrimSpace(opts.SpotifyID) == "" {
		return nil, fmt.Errorf("spotify: missing spotify id")
	}
	secret := opts.TotpSecret
	if secret == "" {
		secret = defaultSpotifyTOTPSecret
	}
	ver := opts.TotpVersion
	if ver == 0 {
		ver = defaultSpotifyTOTPVersion
	}
	c := &spotifyClient{
		http:        opts.httpClient(),
		spDc:        opts.SpDc,
		totpSecret:  secret,
		totpVersion: ver,
		cookies:     map[string]string{},
	}
	if err := c.getSessionInfo(ctx); err != nil {
		return nil, fmt.Errorf("spotify session: %w", err)
	}
	if err := c.getAccessToken(ctx); err != nil {
		return nil, fmt.Errorf("spotify token: %w", err)
	}
	_ = c.getClientToken(ctx) // best-effort; color-lyrics often works with Bearer alone
	return c.colorLyrics(ctx, opts.SpotifyID)
}
