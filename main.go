package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/afkarxyz/SpotiFLAC/backend"
)

var (
	spotifyTrackURLBase = ensureTrailingSlash(envStringDefault("SPOTIFY_TRACK_URL_BASE", "https://open.spotify.com/track"))
	spotifyTrackRegex   = mustCompileRegexFromEnv("SPOTIFY_TRACK_URL_REGEX", `(?i)(?:spotify:track:|https?://open\.spotify\.com/(?:intl-[^/]+/)?track/)([A-Za-z0-9]{22})`)
	spotifyIDRegex      = regexp.MustCompile(`^[A-Za-z0-9]{22}$`)
	normalizeTextRegex  = regexp.MustCompile(`[^a-z0-9]+`)
	validServices       = map[string]struct{}{
		"tidal":  {},
		"qobuz":  {},
		"amazon": {},
	}
	defaultServices = []string{"tidal", "qobuz", "amazon"}
)

const (
	downloadEngineAuto                              = "auto"
	downloadEngineSpotiFLAC                         = "spotiflac"
	downloadEngineMonochrome                        = "monochrome"
	defaultMonochromeTidalAuthURL                   = "https://auth.tidal.com/v1/oauth2/token"
	defaultMonochromeTidalAPIBaseURL                = "https://api.tidal.com/v1"
	defaultMonochromeTidalOpenAPIBaseURL            = "https://openapi.tidal.com/v2"
	defaultMonochromeTidalTrackManifestPathTemplate = "/trackManifests/%d"
	defaultMonochromeTidalPlaybackInfoPathTemplate  = "/tracks/%d/playbackinfo"
	defaultMonochromeTidalClientID                  = "txNoH4kkV41MfH25"
	defaultMonochromeTidalClientSecret              = "dQjy0MinCEvxi1O4UmxvxWnDjt4cgHBPw8ll6nYBk98="
	defaultMonochromeTidalCountryCode               = "US"
	defaultMonochromeDiscoveryPath                  = ""
	defaultMonochromeSearchEndpointPath             = "/search/"
	defaultMonochromeTrackEndpointPath              = "/track/"
	defaultMonochromeTrackManifestsPath             = "/trackManifests/"
)

var validDownloadEngines = map[string]struct{}{
	downloadEngineAuto:       {},
	downloadEngineSpotiFLAC:  {},
	downloadEngineMonochrome: {},
}

var defaultMonochromeAPIInstances = []string{
	"https://hifi.geeked.wtf",
	"https://eu-central.monochrome.tf",
	"https://us-west.monochrome.tf",
	"https://api.monochrome.tf",
	"https://monochrome-api.samidy.com",
	"https://maus.qqdl.site",
	"https://vogel.qqdl.site",
	"https://katze.qqdl.site",
	"https://hund.qqdl.site",
	"https://tidal.kinoplus.online",
	"https://wolf.qqdl.site",
}

var defaultMonochromeDiscoveryURLs = []string{
	"https://tidal-uptime.geeked.wtf/",
}

var defaultMonochromeStreamingInstances = []string{
	"https://hifi.geeked.wtf",
	"https://maus.qqdl.site",
	"https://vogel.qqdl.site",
	"https://katze.qqdl.site",
	"https://hund.qqdl.site",
	"https://wolf.qqdl.site",
}

const defaultMonochromeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36"

type trackMetadata struct {
	SpotifyID   string `json:"spotify_id"`
	Artists     string `json:"artists"`
	Name        string `json:"name"`
	AlbumName   string `json:"album_name"`
	AlbumArtist string `json:"album_artist"`
	ISRC        string `json:"isrc"`
	Images      string `json:"images"`
	ReleaseDate string `json:"release_date"`
	TrackNumber int    `json:"track_number"`
	TotalTracks int    `json:"total_tracks"`
	DiscNumber  int    `json:"disc_number"`
	TotalDiscs  int    `json:"total_discs"`
	Copyright   string `json:"copyright"`
	Publisher   string `json:"publisher"`
}

type trackResponse struct {
	Track trackMetadata `json:"track"`
}

type artist struct {
	Name string `json:"name"`
}

type attempt struct {
	Service string `json:"service"`
	Error   string `json:"error,omitempty"`
}

type createDownloadRequest struct {
	SpotifyURL string   `json:"spotify_url"`
	Services   []string `json:"services,omitempty"`
	TTLSeconds int      `json:"ttl_seconds,omitempty"`
	Engine     string   `json:"engine,omitempty"`
	Method     string   `json:"method,omitempty"`
}

type createDownloadResponse struct {
	OK          bool      `json:"ok"`
	SpotifyID   string    `json:"spotify_id,omitempty"`
	Service     string    `json:"service,omitempty"`
	Method      string    `json:"method,omitempty"`
	Filename    string    `json:"filename,omitempty"`
	DownloadURL string    `json:"download_url,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
	Attempts    []attempt `json:"attempts,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type errorResponse struct {
	OK       bool      `json:"ok"`
	Error    string    `json:"error"`
	Attempts []attempt `json:"attempts,omitempty"`
}

type downloadEntry struct {
	Token     string
	Path      string
	Service   string
	SpotifyID string
	ExpiresAt time.Time
	CreatedAt time.Time
}

type downloadStore struct {
	mu      sync.RWMutex
	entries map[string]downloadEntry
}

func newDownloadStore() *downloadStore {
	return &downloadStore{
		entries: make(map[string]downloadEntry),
	}
}

func (s *downloadStore) put(path, service, spotifyID string, ttl time.Duration) (downloadEntry, error) {
	token, err := generateToken()
	if err != nil {
		return downloadEntry{}, err
	}

	entry := downloadEntry{
		Token:     token,
		Path:      path,
		Service:   service,
		SpotifyID: spotifyID,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(ttl),
	}

	s.mu.Lock()
	s.entries[token] = entry
	s.mu.Unlock()

	return entry, nil
}

func (s *downloadStore) get(token string) (downloadEntry, bool) {
	s.mu.RLock()
	entry, ok := s.entries[token]
	s.mu.RUnlock()
	return entry, ok
}

func (s *downloadStore) delete(token string) {
	s.mu.Lock()
	entry, ok := s.entries[token]
	if ok {
		delete(s.entries, token)
	}
	s.mu.Unlock()

	if ok {
		_ = os.Remove(entry.Path)
		_ = os.Remove(filepath.Dir(entry.Path))
		_ = os.Remove(filepath.Dir(filepath.Dir(entry.Path)))
	}
}

func (s *downloadStore) cleanupExpired() {
	now := time.Now()
	var expired []string

	s.mu.RLock()
	for token, entry := range s.entries {
		if now.After(entry.ExpiresAt) {
			expired = append(expired, token)
		}
	}
	s.mu.RUnlock()

	for _, token := range expired {
		s.delete(token)
	}
}

func (s *downloadStore) startCleanupLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.cleanupExpired()
		case <-ctx.Done():
			return
		}
	}
}

type apiServer struct {
	store             *downloadStore
	baseURL           string
	bindAddr          string
	ttl               time.Duration
	httpClient        *http.Client
	ffmpegAutoInstall bool
	ffmpegMu          sync.Mutex
	ffmpegReady       bool
}

func main() {
	// Prepend custom FFMPEG_PATH and FFPROBE_PATH directories to system PATH to force package-level resolution.
	for _, envVar := range []string{"FFMPEG_PATH", "FFPROBE_PATH"} {
		if pathVal := strings.TrimSpace(os.Getenv(envVar)); pathVal != "" {
			dir := filepath.Dir(pathVal)
			if dir != "." && dir != "/" {
				currentPath := os.Getenv("PATH")
				os.Setenv("PATH", dir+string(os.PathListSeparator)+currentPath)
				log.Printf("Prepended %s dir to PATH: %s", envVar, dir)
			}
		}
	}

	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}

	bindAddr := strings.TrimSpace(os.Getenv("BIND_ADDR"))
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}

	ttl := 2 * time.Hour
	if rawTTL := strings.TrimSpace(os.Getenv("DOWNLOAD_TTL")); rawTTL != "" {
		parsed, err := time.ParseDuration(rawTTL)
		if err != nil {
			log.Fatalf("invalid DOWNLOAD_TTL %q: %v", rawTTL, err)
		}
		if parsed <= 0 {
			log.Fatalf("invalid DOWNLOAD_TTL %q: must be > 0", rawTTL)
		}
		ttl = parsed
	}

	ffmpegAutoInstall := envBoolDefaultTrue("FFMPEG_AUTO_INSTALL")

	server := &apiServer{
		store:             newDownloadStore(),
		baseURL:           strings.TrimSpace(os.Getenv("BASE_URL")),
		bindAddr:          bindAddr,
		ttl:               ttl,
		httpClient:        &http.Client{Timeout: envDurationDefault("HTTP_CLIENT_TIMEOUT", 20*time.Second)},
		ffmpegAutoInstall: ffmpegAutoInstall,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.store.startCleanupLoop(ctx, 1*time.Minute)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", server.handleHealth)
	mux.HandleFunc("/v1/diagnostics", server.handleDiagnostics)
	mux.HandleFunc("/v1/download-url", server.handleCreateDownloadURL)
	mux.HandleFunc("/v1/download/", server.handleDownloadByToken)
	mux.HandleFunc("/", server.handleRoot)

	listenAddr := net.JoinHostPort(bindAddr, port)
	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}

	log.Printf("REST API listening on http://%s", listenAddr)
	log.Printf("Token TTL: %s", ttl)
	log.Printf("FFmpeg auto-install: %t", ffmpegAutoInstall)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

func (s *apiServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, errorResponse{
			OK:    false,
			Error: "route not found",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":      "SpotiFLAC REST API",
		"version":   "1",
		"endpoints": []string{"GET /health", "POST /v1/download-url", "GET /v1/download/{token}"},
	})
}

func (s *apiServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":  true,
		"now": time.Now().UTC(),
	})
}

func (s *apiServer) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	isLocalhost := err == nil && (remoteIP == "127.0.0.1" || remoteIP == "::1" || remoteIP == "localhost")

	token := r.Header.Get("X-Diagnostics-Token")
	expectedToken := strings.TrimSpace(os.Getenv("DIAGNOSTICS_TOKEN"))
	isValidToken := expectedToken != "" && token == expectedToken

	if !isLocalhost && !isValidToken {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	type checkResult struct {
		OK      bool   `json:"ok"`
		Details string `json:"details,omitempty"`
	}

	results := make(map[string]checkResult)
	allOK := true

	// Check ffmpeg
	ffmpegPath, err := backend.GetFFmpegPath()
	if err != nil {
		results["ffmpeg"] = checkResult{OK: false, Details: err.Error()}
		allOK = false
	} else {
		cmd := exec.Command(ffmpegPath, "-version")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			results["ffmpeg"] = checkResult{OK: false, Details: fmt.Sprintf("failed to run: %v", err)}
			allOK = false
		} else {
			firstLine := strings.Split(out.String(), "\n")[0]
			results["ffmpeg"] = checkResult{OK: true, Details: fmt.Sprintf("path: %s, version: %s", ffmpegPath, firstLine)}
		}
	}

	// Check ffprobe
	ffprobePath, err := backend.GetFFprobePath()
	if err != nil {
		results["ffprobe"] = checkResult{OK: false, Details: err.Error()}
		allOK = false
	} else {
		cmd := exec.Command(ffprobePath, "-version")
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			results["ffprobe"] = checkResult{OK: false, Details: fmt.Sprintf("failed to run: %v", err)}
			allOK = false
		} else {
			firstLine := strings.Split(out.String(), "\n")[0]
			results["ffprobe"] = checkResult{OK: true, Details: fmt.Sprintf("path: %s, version: %s", ffprobePath, firstLine)}
		}
	}

	// Check external endpoints
	endpoints := []string{
		"tidal.kinoplus.online",
		"qbz.afkarxyz.qzz.io",
		"amzn.afkarxyz.qzz.io",
		"api.spotify.com",
	}

	for _, host := range endpoints {
		// DNS check
		addrs, err := net.LookupHost(host)
		if err != nil || len(addrs) == 0 {
			results[host+"_dns"] = checkResult{OK: false, Details: fmt.Sprintf("DNS lookup failed: %v", err)}
			allOK = false
			continue
		}
		results[host+"_dns"] = checkResult{OK: true, Details: strings.Join(addrs, ", ")}

		// TCP connectivity to 443
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "443"), 3*time.Second)
		if err != nil {
			results[host+"_tcp"] = checkResult{OK: false, Details: fmt.Sprintf("TCP 443 failed: %v", err)}
			allOK = false
		} else {
			_ = conn.Close()
			results[host+"_tcp"] = checkResult{OK: true, Details: "connected"}
		}
	}

	status := http.StatusOK
	if !allOK {
		status = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":      allOK,
		"results": results,
	})
}

func (s *apiServer) handleCreateDownloadURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{OK: false, Error: "method not allowed"})
		return
	}

	startTime := time.Now()

	var req createDownloadRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Error: "invalid JSON body"})
		return
	}

	if strings.TrimSpace(req.SpotifyURL) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Error: "spotify_url is required"})
		return
	}

	log.Printf("[DOWNLOAD-URL] Request received for URL: %s", req.SpotifyURL)

	engine, err := requestedDownloadEngine(req, r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Error: err.Error()})
		return
	}

	serviceOrder := normalizeServiceOrder(req.Services)
	if len(serviceOrder) == 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Error: "no valid services in services[]"})
		return
	}

	log.Printf("[DOWNLOAD-URL] Resolution started with engine: %s, services: %v", engine, serviceOrder)

	ttl := s.ttl
	if req.TTLSeconds > 0 {
		override := time.Duration(req.TTLSeconds) * time.Second
		if override > 24*time.Hour {
			override = 24 * time.Hour
		}
		ttl = override
	}

	if err := s.ensureFFmpegBinaries(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{OK: false, Error: err.Error()})
		return
	}

	// Dynamic internal timeout (default: 180s)
	timeoutSecs := envIntDefault("DOWNLOAD_URL_TIMEOUT_SECONDS", 180)
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutSecs)*time.Second)
	defer cancel()

	type resolveResult struct {
		downloadPath string
		serviceUsed  string
		methodUsed   string
		spotifyID    string
		attempts     []attempt
		err          error
	}

	resultChan := make(chan resolveResult, 1)
	go func() {
		downloadPath, serviceUsed, methodUsed, spotifyID, attempts, err := s.resolveDownload(ctx, req.SpotifyURL, serviceOrder, engine)
		resultChan <- resolveResult{
			downloadPath: downloadPath,
			serviceUsed:  serviceUsed,
			methodUsed:   methodUsed,
			spotifyID:    spotifyID,
			attempts:     attempts,
			err:          err,
		}
	}()

	var res resolveResult
	select {
	case res = <-resultChan:
		// Done
	case <-ctx.Done():
		elapsedMs := time.Since(startTime).Milliseconds()
		log.Printf("[DOWNLOAD-URL] Request timed out after %d ms (limit: %ds)", elapsedMs, timeoutSecs)
		writeJSON(w, http.StatusGatewayTimeout, errorResponse{
			OK:    false,
			Error: fmt.Sprintf("request timed out after %ds", timeoutSecs),
		})
		return
	}

	if res.err != nil {
		elapsedMs := time.Since(startTime).Milliseconds()
		log.Printf("[DOWNLOAD-URL] Request failed after %d ms: %v", elapsedMs, res.err)
		writeJSON(w, http.StatusBadGateway, createDownloadResponse{
			OK:       false,
			Error:    res.err.Error(),
			Attempts: res.attempts,
		})
		return
	}

	entry, err := s.store.put(res.downloadPath, res.serviceUsed, res.spotifyID, ttl)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{OK: false, Error: "failed to generate download token"})
		return
	}

	downloadURL := fmt.Sprintf("%s/v1/download/%s", s.publicBaseURL(r), entry.Token)
	elapsedMs := time.Since(startTime).Milliseconds()
	log.Printf("[DOWNLOAD-URL] Request completed in %d ms (download_url: %s)", elapsedMs, downloadURL)

	writeJSON(w, http.StatusOK, createDownloadResponse{
		OK:          true,
		SpotifyID:   res.spotifyID,
		Service:     res.serviceUsed,
		Method:      res.methodUsed,
		Filename:    filepath.Base(entry.Path),
		DownloadURL: downloadURL,
		ExpiresAt:   entry.ExpiresAt.UTC(),
		Attempts:    res.attempts,
	})
}

func (s *apiServer) handleDownloadByToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{OK: false, Error: "method not allowed"})
		return
	}

	prefix := "/v1/download/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeJSON(w, http.StatusNotFound, errorResponse{OK: false, Error: "route not found"})
		return
	}

	token := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, prefix))
	if token == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{OK: false, Error: "missing token"})
		return
	}

	entry, ok := s.store.get(token)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{OK: false, Error: "invalid or expired token"})
		return
	}

	if time.Now().After(entry.ExpiresAt) {
		s.store.delete(token)
		writeJSON(w, http.StatusGone, errorResponse{OK: false, Error: "download token expired"})
		return
	}

	file, err := os.Open(entry.Path)
	if err != nil {
		s.store.delete(token)
		writeJSON(w, http.StatusNotFound, errorResponse{OK: false, Error: "file no longer available"})
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{OK: false, Error: "unable to read file metadata"})
		return
	}

	filename := filepath.Base(entry.Path)
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename)))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, filename, info.ModTime(), file)
}

type monochromeClient struct {
	httpClient                 *http.Client
	apiInstances               []string
	streamingInstances         []string
	discoveryURLs              []string
	explicitAPIInstances       bool
	explicitStreamingInstances bool
	discoveryPath              string
	searchEndpointPath         string
	trackEndpointPath          string
	trackManifestsPath         string
	tidalAuthURL               string
	tidalAPIBaseURL            string
	tidalOpenAPIBaseURL        string
	tidalTrackManifestPath     string
	tidalPlaybackInfoPath      string
	tidalClientID              string
	tidalClientSecret          string
	tidalCountryCode           string
	tokenMu                    sync.Mutex
	token                      string
	tokenExpiry                time.Time
	discoveryOnce              sync.Once
}

type monochromeSearchResponse struct {
	Data struct {
		Items []monochromeTrack `json:"items"`
	} `json:"data"`
}

type monochromeTrackManifestResponse struct {
	Data struct {
		Data struct {
			Attributes struct {
				URI string `json:"uri"`
			} `json:"attributes"`
		} `json:"data"`
	} `json:"data"`
}

type monochromePlaybackInfo struct {
	URL              string `json:"url"`
	StreamURL        string `json:"streamUrl"`
	OriginalTrackURL string `json:"OriginalTrackUrl"`
	Manifest         string `json:"manifest"`
	ManifestURL      string `json:"manifestUrl"`
	ManifestMimeType string `json:"manifestMimeType"`
}

type monochromeTrackRouteResponse struct {
	Data monochromePlaybackInfo `json:"data"`
}

type monochromeTidalTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

type monochromeInstancesPayload struct {
	API       []monochromeInstance `json:"api"`
	Streaming []monochromeInstance `json:"streaming"`
}

type monochromeInstance struct {
	URL string `json:"url"`
}

type monochromeTrack struct {
	ID           int64    `json:"id"`
	Title        string   `json:"title"`
	Version      string   `json:"version"`
	ISRC         string   `json:"isrc"`
	AudioQuality string   `json:"audioQuality"`
	StreamReady  bool     `json:"streamReady"`
	Artists      []artist `json:"artists"`
	Album        struct {
		Title string `json:"title"`
	} `json:"album"`
}

func requestedDownloadEngine(req createDownloadRequest, r *http.Request) (string, error) {
	engine := firstNonEmpty(
		strings.TrimSpace(r.URL.Query().Get("engine")),
		strings.TrimSpace(r.URL.Query().Get("method")),
		strings.TrimSpace(req.Engine),
		strings.TrimSpace(req.Method),
	)
	if engine == "" {
		return downloadEngineAuto, nil
	}

	engine = strings.ToLower(engine)
	if _, ok := validDownloadEngines[engine]; !ok {
		return "", fmt.Errorf("invalid engine %q: valid values are auto, spotiflac, monochrome", engine)
	}

	return engine, nil
}

func (s *apiServer) resolveDownload(ctx context.Context, spotifyInput string, serviceOrder []string, engine string) (downloadPath, serviceUsed, methodUsed, spotifyID string, attempts []attempt, err error) {
	spotifyID, err = extractSpotifyTrackID(spotifyInput)
	if err != nil {
		return "", "", "", "", nil, err
	}

	spotifyURL := spotifyTrackURLBase + spotifyID
	meta, err := fetchTrackMetadata(spotifyURL)
	if err != nil {
		return "", "", "", spotifyID, nil, fmt.Errorf("failed to fetch Spotify metadata: %w", err)
	}

	workDir, err := os.MkdirTemp("", "spotiflac-rest-")
	if err != nil {
		return "", "", "", spotifyID, nil, fmt.Errorf("failed to create temp directory: %w", err)
	}
	cleanupWorkDir := true
	defer func() {
		if cleanupWorkDir {
			_ = os.RemoveAll(workDir)
		}
	}()

	switch engine {
	case downloadEngineSpotiFLAC:
		downloadPath, serviceUsed, attempts, err = s.resolveWithSpotiFLAC(spotifyID, spotifyURL, meta, serviceOrder, workDir)
		if err != nil {
			return "", "", "", spotifyID, attempts, err
		}
		cleanupWorkDir = false
		return downloadPath, serviceUsed, downloadEngineSpotiFLAC, spotifyID, attempts, nil
	case downloadEngineMonochrome:
		downloadPath, attempts, err = s.resolveWithMonochrome(ctx, meta, workDir)
		if err != nil {
			return "", "", "", spotifyID, attempts, err
		}
		cleanupWorkDir = false
		return downloadPath, downloadEngineMonochrome, downloadEngineMonochrome, spotifyID, attempts, nil
	default:
		downloadPath, serviceUsed, attempts, err = s.resolveWithSpotiFLAC(spotifyID, spotifyURL, meta, serviceOrder, workDir)
		if err == nil {
			cleanupWorkDir = false
			return downloadPath, serviceUsed, downloadEngineSpotiFLAC, spotifyID, attempts, nil
		}

		monochromePath, monochromeAttempts, monochromeErr := s.resolveWithMonochrome(ctx, meta, workDir)
		attempts = append(attempts, monochromeAttempts...)
		if monochromeErr == nil {
			cleanupWorkDir = false
			return monochromePath, downloadEngineMonochrome, downloadEngineMonochrome, spotifyID, attempts, nil
		}

		return "", "", "", spotifyID, attempts, fmt.Errorf("spotiflac failed: %v; monochrome failed: %w", err, monochromeErr)
	}
}

func (s *apiServer) resolveWithSpotiFLAC(spotifyID, spotifyURL string, meta trackMetadata, serviceOrder []string, workDir string) (downloadPath, serviceUsed string, attempts []attempt, err error) {
	for _, service := range serviceOrder {
		serviceDir := filepath.Join(workDir, service)
		filename, dlErr := runServiceDownload(service, spotifyID, spotifyURL, meta, serviceDir)
		if dlErr != nil {
			attempts = append(attempts, attempt{Service: service, Error: dlErr.Error()})
			continue
		}

		filename = strings.TrimPrefix(filename, "EXISTS:")
		if filename == "" {
			attempts = append(attempts, attempt{Service: service, Error: "empty file path returned"})
			continue
		}

		stat, statErr := os.Stat(filename)
		if statErr != nil {
			attempts = append(attempts, attempt{Service: service, Error: fmt.Sprintf("downloaded file missing: %v", statErr)})
			continue
		}
		if stat.Size() <= 0 {
			attempts = append(attempts, attempt{Service: service, Error: "downloaded file is empty"})
			continue
		}

		attempts = append(attempts, attempt{Service: service})
		return filename, service, attempts, nil
	}

	return "", "", attempts, fmt.Errorf("failed in all services: %s", strings.Join(serviceOrder, " -> "))
}

func (s *apiServer) resolveWithMonochrome(ctx context.Context, meta trackMetadata, workDir string) (string, []attempt, error) {
	attempts := make([]attempt, 0, 1)

	client := newMonochromeClient(s.httpClient)
	track, err := client.searchTrack(ctx, meta)
	if err != nil {
		attempts = append(attempts, attempt{Service: downloadEngineMonochrome, Error: err.Error()})
		return "", attempts, err
	}

	manifestURI, err := client.getTrackManifestURI(ctx, track.ID)
	if err != nil {
		attempts = append(attempts, attempt{Service: downloadEngineMonochrome, Error: err.Error()})
		return "", attempts, err
	}

	outputPath := filepath.Join(workDir, downloadEngineMonochrome, buildMonochromeFilename(meta))
	if err := downloadMonochromeTrack(ctx, manifestURI, outputPath, meta); err != nil {
		attempts = append(attempts, attempt{Service: downloadEngineMonochrome, Error: err.Error()})
		return "", attempts, err
	}

	attempts = append(attempts, attempt{Service: downloadEngineMonochrome})
	return outputPath, attempts, nil
}

func runServiceDownload(service, spotifyID, spotifyURL string, meta trackMetadata, outputDir string) (string, error) {
	switch service {
	case "tidal":
		downloader := backend.NewTidalDownloader("")
		return downloader.Download(
			spotifyID,
			outputDir,
			"LOSSLESS",
			"title-artist",
			false,
			0,
			meta.Name,
			meta.Artists,
			meta.AlbumName,
			meta.AlbumArtist,
			meta.ReleaseDate,
			false,
			meta.Images,
			true,
			meta.TrackNumber,
			meta.DiscNumber,
			meta.TotalTracks,
			meta.TotalDiscs,
			meta.Copyright,
			meta.Publisher,
			"",
			"",
			meta.ISRC,
			spotifyURL,
			true,
			false,
			false,
			false,
		)

	case "qobuz":
		downloader := backend.NewQobuzDownloader()
		return downloader.DownloadTrack(
			spotifyID,
			outputDir,
			"6",
			"title-artist",
			false,
			0,
			meta.Name,
			meta.Artists,
			meta.AlbumName,
			meta.AlbumArtist,
			meta.ReleaseDate,
			false,
			meta.Images,
			true,
			meta.TrackNumber,
			meta.DiscNumber,
			meta.TotalTracks,
			meta.TotalDiscs,
			meta.Copyright,
			meta.Publisher,
			"",
			"",
			spotifyURL,
			true,
			false,
			false,
			false,
		)

	case "amazon":
		downloader := backend.NewAmazonDownloader()
		return downloader.DownloadBySpotifyID(
			spotifyID,
			outputDir,
			"flac",
			"title-artist",
			"",
			"",
			false,
			0,
			meta.Name,
			meta.Artists,
			meta.AlbumName,
			meta.AlbumArtist,
			meta.ReleaseDate,
			meta.Images,
			meta.TrackNumber,
			meta.DiscNumber,
			meta.TotalTracks,
			true,
			meta.TotalDiscs,
			meta.Copyright,
			meta.Publisher,
			"",
			"",
			meta.ISRC,
			spotifyURL,
			false,
			false,
			false,
		)
	default:
		return "", fmt.Errorf("unsupported service: %s", service)
	}
}

func newMonochromeClient(httpClient *http.Client) *monochromeClient {
	apiInstances, explicitAPIInstances := splitCSVEnvWithSource("MONOCHROME_API_INSTANCES", defaultMonochromeAPIInstances)
	streamingInstances, explicitStreamingInstances := splitCSVEnvWithSource("MONOCHROME_STREAMING_INSTANCES", defaultMonochromeStreamingInstances)

	return &monochromeClient{
		httpClient:                 httpClient,
		apiInstances:               apiInstances,
		streamingInstances:         streamingInstances,
		discoveryURLs:              splitCSVEnv("MONOCHROME_DISCOVERY_URLS", defaultMonochromeDiscoveryURLs),
		explicitAPIInstances:       explicitAPIInstances,
		explicitStreamingInstances: explicitStreamingInstances,
		discoveryPath:              defaultMonochromeDiscoveryPath,
		searchEndpointPath:         defaultMonochromeSearchEndpointPath,
		trackEndpointPath:          defaultMonochromeTrackEndpointPath,
		trackManifestsPath:         defaultMonochromeTrackManifestsPath,
		tidalAuthURL:               defaultMonochromeTidalAuthURL,
		tidalAPIBaseURL:            defaultMonochromeTidalAPIBaseURL,
		tidalOpenAPIBaseURL:        defaultMonochromeTidalOpenAPIBaseURL,
		tidalTrackManifestPath:     defaultMonochromeTidalTrackManifestPathTemplate,
		tidalPlaybackInfoPath:      defaultMonochromeTidalPlaybackInfoPathTemplate,
		tidalClientID:              envStringDefault("MONOCHROME_TIDAL_CLIENT_ID", defaultMonochromeTidalClientID),
		tidalClientSecret:          envStringDefault("MONOCHROME_TIDAL_CLIENT_SECRET", defaultMonochromeTidalClientSecret),
		tidalCountryCode:           defaultMonochromeTidalCountryCode,
	}
}

func (c *monochromeClient) searchTrack(ctx context.Context, meta trackMetadata) (monochromeTrack, error) {
	c.loadDiscoveredInstances(ctx)

	queries := monochromeSearchQueries(meta)
	bestScore := -1
	var bestTrack monochromeTrack
	var lastErr error

	for _, query := range queries {
		for _, instance := range c.apiInstances {
			var payload monochromeSearchResponse
			if err := c.getJSON(ctx, instance, c.searchEndpointPath+"?s="+url.QueryEscape(query), &payload); err != nil {
				lastErr = err
				continue
			}

			track, score, ok := bestMonochromeCandidate(payload.Data.Items, meta)
			if !ok {
				continue
			}
			if score > bestScore {
				bestScore = score
				bestTrack = track
			}
		}
		if bestScore >= 120 {
			return bestTrack, nil
		}
	}

	if bestScore >= 0 {
		return bestTrack, nil
	}
	if lastErr != nil {
		return monochromeTrack{}, fmt.Errorf("monochrome search failed: %w", lastErr)
	}
	return monochromeTrack{}, fmt.Errorf("monochrome search returned no matching track")
}

func (c *monochromeClient) getTrackManifestURI(ctx context.Context, trackID int64) (string, error) {
	c.loadDiscoveredInstances(ctx)

	if uri, err := c.getOfficialTrackManifestURI(ctx, trackID); err == nil && strings.TrimSpace(uri) != "" {
		return uri, nil
	}

	params := url.Values{}
	params.Set("id", fmt.Sprintf("%d", trackID))
	params.Add("formats", "FLAC")
	params.Add("formats", "FLAC_HIRES")
	params.Set("adaptive", "true")
	params.Set("manifestType", "MPEG_DASH")
	params.Set("uriScheme", "HTTPS")
	params.Set("usage", "PLAYBACK")

	var lastErr error
	for _, instance := range c.streamingInstances {
		var payload monochromeTrackManifestResponse
		if err := c.getJSON(ctx, instance, c.trackManifestsPath+"?"+params.Encode(), &payload); err != nil {
			lastErr = err
			continue
		}
		uri := strings.TrimSpace(payload.Data.Data.Attributes.URI)
		if uri != "" {
			return uri, nil
		}
		lastErr = fmt.Errorf("empty uri from %s", instance)
	}

	if uri, err := c.getOfficialPlaybackInfoURI(ctx, trackID); err == nil && strings.TrimSpace(uri) != "" {
		return uri, nil
	} else if err != nil {
		lastErr = err
	}

	for _, instance := range c.apiInstances {
		var payload monochromeTrackRouteResponse
		if err := c.getJSON(ctx, instance, fmt.Sprintf("%s?id=%d&quality=LOSSLESS", c.trackEndpointPath, trackID), &payload); err != nil {
			lastErr = err
			continue
		}

		uri, err := resolvePlaybackInfoURI(payload.Data)
		if err != nil {
			lastErr = err
			continue
		}
		return uri, nil
	}

	if lastErr != nil {
		return "", fmt.Errorf("monochrome manifest lookup failed: %w", lastErr)
	}
	return "", fmt.Errorf("monochrome manifest lookup failed")
}

func (c *monochromeClient) loadDiscoveredInstances(ctx context.Context) {
	c.discoveryOnce.Do(func() {
		if c.explicitAPIInstances && c.explicitStreamingInstances {
			return
		}

		discoveredAPI, discoveredStreaming := c.discoverInstances(ctx)
		if !c.explicitAPIInstances && len(discoveredAPI) > 0 {
			c.apiInstances = prioritizeMonochromeInstances(mergeUniqueStrings(c.apiInstances, discoveredAPI))
		}
		if !c.explicitStreamingInstances {
			if len(discoveredStreaming) > 0 {
				c.streamingInstances = prioritizeMonochromeInstances(mergeUniqueStrings(c.streamingInstances, discoveredStreaming))
			} else if len(discoveredAPI) > 0 {
				c.streamingInstances = prioritizeMonochromeInstances(mergeUniqueStrings(c.streamingInstances, discoveredAPI))
			}
		}
	})
}

func (c *monochromeClient) discoverInstances(ctx context.Context) ([]string, []string) {
	for _, discoveryURL := range c.discoveryURLs {
		var payload monochromeInstancesPayload
		if err := c.getJSON(ctx, discoveryURL, c.discoveryPath, &payload); err != nil {
			continue
		}

		apiInstances := normalizeMonochromeInstanceURLs(payload.API)
		streamingInstances := normalizeMonochromeInstanceURLs(payload.Streaming)
		if len(apiInstances) == 0 && len(streamingInstances) == 0 {
			continue
		}
		return apiInstances, streamingInstances
	}

	return nil, nil
}

func (c *monochromeClient) getOfficialTrackManifestURI(ctx context.Context, trackID int64) (string, error) {
	token, err := c.getTidalToken(ctx)
	if err != nil {
		return "", err
	}

	params := url.Values{}
	params.Set("adaptive", "true")
	params.Set("manifestType", "MPEG_DASH")
	params.Set("uriScheme", "HTTPS")
	params.Set("usage", "PLAYBACK")
	params.Add("formats", "FLAC")
	params.Add("formats", "FLAC_HIRES")

	requestURL := joinBaseURLAndPath(c.tidalOpenAPIBaseURL, fmt.Sprintf(c.tidalTrackManifestPath, trackID)) + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	var payload monochromeTrackManifestResponse
	if err := c.doJSON(req, &payload); err != nil {
		return "", err
	}

	uri := strings.TrimSpace(payload.Data.Data.Attributes.URI)
	if uri == "" {
		return "", fmt.Errorf("official track manifest returned empty uri")
	}
	return uri, nil
}

func (c *monochromeClient) getOfficialPlaybackInfoURI(ctx context.Context, trackID int64) (string, error) {
	token, err := c.getTidalToken(ctx)
	if err != nil {
		return "", err
	}

	params := url.Values{}
	params.Set("audioquality", "LOSSLESS")
	params.Set("playbackmode", "STREAM")
	params.Set("assetpresentation", "FULL")
	params.Set("countryCode", c.tidalCountryCode)
	params.Set("immersiveAudio", "false")

	requestURL := joinBaseURLAndPath(c.tidalAPIBaseURL, fmt.Sprintf(c.tidalPlaybackInfoPath, trackID)) + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	var payload monochromePlaybackInfo
	if err := c.doJSON(req, &payload); err != nil {
		return "", err
	}

	return resolvePlaybackInfoURI(payload)
}

func (c *monochromeClient) getTidalToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	if c.token != "" && time.Now().Before(c.tokenExpiry) {
		return c.token, nil
	}

	form := url.Values{}
	form.Set("client_id", c.tidalClientID)
	form.Set("client_secret", c.tidalClientSecret)
	form.Set("grant_type", "client_credentials")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tidalAuthURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.tidalClientID+":"+c.tidalClientSecret)))

	var payload monochromeTidalTokenResponse
	if err := c.doJSON(req, &payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return "", fmt.Errorf("official tidal auth returned empty access token")
	}

	expirySeconds := payload.ExpiresIn
	if expirySeconds <= 0 {
		expirySeconds = 3600
	}
	c.token = strings.TrimSpace(payload.AccessToken)
	c.tokenExpiry = time.Now().Add(time.Duration(expirySeconds-60) * time.Second)
	return c.token, nil
}

func (c *monochromeClient) getJSON(ctx context.Context, baseURL, relativePath string, target any) error {
	requestURL := joinBaseURLAndPath(baseURL, relativePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return err
	}

	return c.doJSON(req, target)
}

func (c *monochromeClient) doJSON(req *http.Request, target any) error {
	if strings.TrimSpace(req.Header.Get("User-Agent")) == "" {
		req.Header.Set("User-Agent", defaultMonochromeUserAgent)
	}
	if strings.TrimSpace(req.Header.Get("Accept")) == "" {
		req.Header.Set("Accept", "application/json, text/plain, */*")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		if len(body) == 0 {
			return fmt.Errorf("unexpected HTTP %d from %s", resp.StatusCode, req.URL.String())
		}
		return fmt.Errorf("unexpected HTTP %d from %s: %s", resp.StatusCode, req.URL.String(), strings.TrimSpace(string(body)))
	}

	return json.NewDecoder(resp.Body).Decode(target)
}

func resolvePlaybackInfoURI(payload monochromePlaybackInfo) (string, error) {
	for _, candidate := range []string{payload.URL, payload.StreamURL, payload.OriginalTrackURL, payload.ManifestURL} {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" {
			return candidate, nil
		}
	}

	manifest := strings.TrimSpace(payload.Manifest)
	if manifest == "" {
		return "", fmt.Errorf("playback info did not include stream url or manifest")
	}

	decoded, err := decodePlaybackManifest(manifest)
	if err != nil {
		return "", err
	}

	if strings.Contains(decoded, "<MPD") {
		return "inline-dash:" + base64.StdEncoding.EncodeToString([]byte(decoded)), nil
	}

	var manifestPayload struct {
		URLs []string `json:"urls"`
	}
	if json.Unmarshal([]byte(decoded), &manifestPayload) == nil && len(manifestPayload.URLs) > 0 {
		return strings.TrimSpace(manifestPayload.URLs[0]), nil
	}

	match := regexp.MustCompile(`https?://[^\s"'<>]+`).FindString(decoded)
	if match != "" {
		return match, nil
	}

	return "", fmt.Errorf("unable to resolve stream URL from playback manifest")
}

func decodePlaybackManifest(manifest string) (string, error) {
	if manifest == "" {
		return "", fmt.Errorf("empty playback manifest")
	}

	decoded, err := base64.StdEncoding.DecodeString(manifest)
	if err == nil {
		return string(decoded), nil
	}

	return manifest, nil
}

func bestMonochromeCandidate(candidates []monochromeTrack, meta trackMetadata) (monochromeTrack, int, bool) {
	bestScore := -1
	var best monochromeTrack

	for _, candidate := range candidates {
		score := scoreMonochromeCandidate(candidate, meta)
		if score > bestScore {
			bestScore = score
			best = candidate
		}
	}

	if bestScore < 120 {
		return monochromeTrack{}, 0, false
	}

	return best, bestScore, true
}

func scoreMonochromeCandidate(candidate monochromeTrack, meta trackMetadata) int {
	score := 0

	expectedTitle := normalizeComparableText(meta.Name)
	candidateTitle := normalizeComparableText(monochromeTrackTitle(candidate))
	if expectedTitle != "" && candidateTitle == expectedTitle {
		score += 100
	} else if expectedTitle != "" && (strings.Contains(candidateTitle, expectedTitle) || strings.Contains(expectedTitle, candidateTitle)) {
		score += 55
	}

	expectedArtists := normalizedArtists(meta.Artists)
	candidateArtists := normalizedArtists(joinArtistNames(candidate.Artists))
	for _, expected := range expectedArtists {
		for _, actual := range candidateArtists {
			if expected == actual {
				score += 60
				goto artistDone
			}
			if strings.Contains(actual, expected) || strings.Contains(expected, actual) {
				score += 30
				goto artistDone
			}
		}
	}
artistDone:

	expectedAlbum := normalizeComparableText(meta.AlbumName)
	candidateAlbum := normalizeComparableText(candidate.Album.Title)
	if expectedAlbum != "" && candidateAlbum == expectedAlbum {
		score += 20
	}

	if meta.ISRC != "" && strings.EqualFold(strings.TrimSpace(meta.ISRC), strings.TrimSpace(candidate.ISRC)) {
		score += 100
	}

	if candidate.StreamReady {
		score += 10
	}
	if strings.Contains(strings.ToUpper(candidate.AudioQuality), "LOSSLESS") {
		score += 5
	}

	if strings.Contains(candidateTitle, "remix") != strings.Contains(expectedTitle, "remix") {
		score -= 20
	}

	return score
}

func monochromeSearchQueries(meta trackMetadata) []string {
	queries := []string{
		strings.TrimSpace(meta.Name + " " + firstArtist(meta.Artists)),
		strings.TrimSpace(meta.Name + " " + meta.Artists),
	}
	if meta.ISRC != "" {
		queries = append([]string{"isrc:" + strings.TrimSpace(meta.ISRC)}, queries...)
	}

	seen := make(map[string]struct{})
	filtered := make([]string, 0, len(queries))
	for _, query := range queries {
		query = strings.TrimSpace(query)
		if query == "" {
			continue
		}
		if _, ok := seen[query]; ok {
			continue
		}
		seen[query] = struct{}{}
		filtered = append(filtered, query)
	}
	return filtered
}

func monochromeTrackTitle(track monochromeTrack) string {
	title := strings.TrimSpace(track.Title)
	version := strings.TrimSpace(track.Version)
	if title == "" {
		return ""
	}
	if version == "" {
		return title
	}
	return title + " " + version
}

func joinArtistNames(artists []artist) string {
	names := make([]string, 0, len(artists))
	for _, artist := range artists {
		name := strings.TrimSpace(artist.Name)
		if name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}

func normalizedArtists(input string) []string {
	parts := strings.Split(input, ",")
	normalized := make([]string, 0, len(parts))
	for _, part := range parts {
		value := normalizeComparableText(part)
		if value != "" {
			normalized = append(normalized, value)
		}
	}
	return normalized
}

func normalizeComparableText(input string) string {
	input = strings.ToLower(strings.TrimSpace(input))
	if input == "" {
		return ""
	}
	input = normalizeTextRegex.ReplaceAllString(input, " ")
	return strings.Join(strings.Fields(input), " ")
}

func downloadMonochromeTrack(ctx context.Context, manifestURI, outputPath string, meta trackMetadata) error {
	ffmpegPath, err := backend.GetFFmpegPath()
	if err != nil {
		return fmt.Errorf("failed to resolve ffmpeg path: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return err
	}

	input := manifestURI
	if strings.HasPrefix(manifestURI, "inline-dash:") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(manifestURI, "inline-dash:"))
		if err != nil {
			return fmt.Errorf("failed to decode inline dash manifest: %w", err)
		}

		manifestPath := filepath.Join(filepath.Dir(outputPath), "manifest.mpd")
		if err := os.WriteFile(manifestPath, decoded, 0o644); err != nil {
			return fmt.Errorf("failed to write inline dash manifest: %w", err)
		}
		input = manifestPath
	}

	args := []string{
		"-y",
		"-loglevel", "error",
		"-protocol_whitelist", "file,http,https,tcp,tls,crypto,data",
		"-i", input,
		"-vn",
		"-map", "0:a:0",
		"-c:a", "flac",
	}
	args = append(args, ffmpegMetadataArgs(meta)...)
	args = append(args, outputPath)

	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("ffmpeg monochrome download failed: %s", message)
	}

	return nil
}

func ffmpegMetadataArgs(meta trackMetadata) []string {
	args := make([]string, 0, 20)
	appendMeta := func(key, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		args = append(args, "-metadata", key+"="+value)
	}

	appendMeta("title", meta.Name)
	appendMeta("artist", meta.Artists)
	appendMeta("album", meta.AlbumName)
	appendMeta("album_artist", meta.AlbumArtist)
	appendMeta("date", meta.ReleaseDate)
	appendMeta("copyright", meta.Copyright)
	appendMeta("publisher", meta.Publisher)
	if meta.TrackNumber > 0 {
		appendMeta("track", fmt.Sprintf("%d/%d", meta.TrackNumber, maxInt(meta.TotalTracks, meta.TrackNumber)))
	}
	if meta.DiscNumber > 0 {
		appendMeta("disc", fmt.Sprintf("%d/%d", meta.DiscNumber, maxInt(meta.TotalDiscs, meta.DiscNumber)))
	}

	return args
}

func buildMonochromeFilename(meta trackMetadata) string {
	base := strings.TrimSpace(meta.Name)
	artist := strings.TrimSpace(firstArtist(meta.Artists))
	if base == "" {
		base = "track"
	}
	if artist != "" {
		base += " - " + artist
	}

	var cleaned strings.Builder
	for _, r := range base {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			cleaned.WriteRune('_')
		default:
			if r < 32 {
				cleaned.WriteRune('_')
			} else {
				cleaned.WriteRune(r)
			}
		}
	}

	filename := strings.Join(strings.Fields(cleaned.String()), " ")
	filename = strings.Trim(filename, ". ")
	if filename == "" {
		filename = "track"
	}

	return filename + ".flac"
}

func splitCSVEnv(name string, defaults []string) []string {
	values, _ := splitCSVEnvWithSource(name, defaults)
	return values
}

func splitCSVEnvWithSource(name string, defaults []string) ([]string, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return append([]string(nil), defaults...), false
	}

	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			values = append(values, strings.TrimRight(part, "/"))
		}
	}
	if len(values) == 0 {
		return append([]string(nil), defaults...), false
	}
	return values, true
}

func envStringDefault(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func envIntDefault(key string, defaultVal int) int {
	valStr := strings.TrimSpace(os.Getenv(key))
	if valStr == "" {
		return defaultVal
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		log.Printf("Warning: invalid int value for %s: %s (using default %d)", key, valStr, defaultVal)
		return defaultVal
	}
	return val
}

func envDurationDefault(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}

	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}

	return parsed
}

func mustCompileRegexFromEnv(name, fallback string) *regexp.Regexp {
	pattern := envStringDefault(name, fallback)
	re, err := regexp.Compile(pattern)
	if err != nil {
		panic(fmt.Sprintf("invalid regex in %s: %v", name, err))
	}
	return re
}

func ensureTrailingSlash(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return strings.TrimRight(value, "/") + "/"
}

func joinBaseURLAndPath(baseURL, relativePath string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	relativePath = strings.TrimSpace(relativePath)
	if relativePath == "" {
		return baseURL
	}
	if !strings.HasPrefix(relativePath, "/") {
		relativePath = "/" + relativePath
	}
	return baseURL + relativePath
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstArtist(artists string) string {
	parts := strings.Split(artists, ",")
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func normalizeMonochromeInstanceURLs(instances []monochromeInstance) []string {
	values := make([]string, 0, len(instances))
	seen := make(map[string]struct{})

	for _, instance := range instances {
		value := strings.TrimRight(strings.TrimSpace(instance.URL), "/")
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}

	return values
}

func mergeUniqueStrings(base []string, extra []string) []string {
	values := make([]string, 0, len(base)+len(extra))
	seen := make(map[string]struct{}, len(base)+len(extra))

	appendValue := func(value string) {
		value = strings.TrimRight(strings.TrimSpace(value), "/")
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}

	for _, value := range base {
		appendValue(value)
	}
	for _, value := range extra {
		appendValue(value)
	}

	return values
}

func prioritizeMonochromeInstances(instances []string) []string {
	preferred := make([]string, 0, len(instances))
	deprioritized := make([]string, 0, len(instances))

	for _, instance := range instances {
		if strings.Contains(instance, ".qqdl.site") {
			deprioritized = append(deprioritized, instance)
			continue
		}
		preferred = append(preferred, instance)
	}

	return append(preferred, deprioritized...)
}

func fetchTrackMetadata(spotifyURL string) (trackMetadata, error) {
	ctx, cancel := context.WithTimeout(context.Background(), envDurationDefault("SPOTIFY_METADATA_TIMEOUT", 45*time.Second))
	defer cancel()

	data, err := backend.GetFilteredSpotifyData(ctx, spotifyURL, false, 0, "", nil)
	if err != nil {
		return trackMetadata{}, err
	}

	raw, err := json.Marshal(data)
	if err != nil {
		return trackMetadata{}, err
	}

	var payload trackResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return trackMetadata{}, err
	}

	if strings.TrimSpace(payload.Track.Name) == "" {
		return trackMetadata{}, fmt.Errorf("spotify metadata did not include track name")
	}

	if payload.Track.ISRC == "" && strings.TrimSpace(payload.Track.SpotifyID) != "" {
		songLinkClient := backend.NewSongLinkClient()
		if isrc, isrcErr := songLinkClient.GetISRCDirect(strings.TrimSpace(payload.Track.SpotifyID)); isrcErr == nil {
			payload.Track.ISRC = strings.TrimSpace(isrc)
		}
	}

	return payload.Track, nil
}

func extractSpotifyTrackID(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("spotify URL is empty")
	}

	if spotifyIDRegex.MatchString(input) {
		return input, nil
	}

	matches := spotifyTrackRegex.FindStringSubmatch(input)
	if len(matches) < 2 {
		return "", fmt.Errorf("invalid Spotify track URL or ID")
	}

	return matches[1], nil
}

func normalizeServiceOrder(services []string) []string {
	if len(services) == 0 {
		return append([]string(nil), defaultServices...)
	}

	seen := make(map[string]struct{})
	normalized := make([]string, 0, len(services))

	for _, service := range services {
		service = strings.ToLower(strings.TrimSpace(service))
		if _, ok := validServices[service]; !ok {
			continue
		}
		if _, exists := seen[service]; exists {
			continue
		}
		seen[service] = struct{}{}
		normalized = append(normalized, service)
	}

	return normalized
}

func (s *apiServer) publicBaseURL(r *http.Request) string {
	if s.baseURL != "" {
		return strings.TrimRight(s.baseURL, "/")
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwardedProto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwardedProto != "" {
		scheme = forwardedProto
	}

	return fmt.Sprintf("%s://%s", scheme, r.Host)
}

func (s *apiServer) ensureFFmpegBinaries() error {
	if !s.ffmpegAutoInstall {
		return nil
	}

	s.ffmpegMu.Lock()
	defer s.ffmpegMu.Unlock()

	if s.ffmpegReady {
		return nil
	}

	ffmpegInstalled, err := backend.IsFFmpegInstalled()
	if err != nil {
		log.Printf("FFmpeg check error: %v", err)
	}

	ffprobeInstalled, err := backend.IsFFprobeInstalled()
	if err != nil {
		log.Printf("FFprobe check error: %v", err)
	}

	if ffmpegInstalled && ffprobeInstalled {
		s.ffmpegReady = true
		return nil
	}

	homeDir, homeSource, err := ensureHomeEnv()
	if err != nil {
		return fmt.Errorf("failed to prepare HOME for ffmpeg bootstrap: %w", err)
	}
	if homeSource == "user-home" {
		log.Printf("HOME is not defined; using os.UserHomeDir()=%s for ffmpeg bootstrap", homeDir)
	}
	if homeSource == "fallback" {
		log.Printf("HOME is not defined; using fallback HOME=%s for ffmpeg bootstrap", homeDir)
	}

	log.Printf("FFmpeg/FFprobe not available, auto-installing...")
	if err := backend.DownloadFFmpeg(nil); err != nil {
		return fmt.Errorf("failed to auto-install ffmpeg: %w", err)
	}

	ffmpegInstalled, _ = backend.IsFFmpegInstalled()
	ffprobeInstalled, _ = backend.IsFFprobeInstalled()
	if !ffmpegInstalled || !ffprobeInstalled {
		return fmt.Errorf("ffmpeg bootstrap incomplete (ffmpeg=%t, ffprobe=%t)", ffmpegInstalled, ffprobeInstalled)
	}

	s.ffmpegReady = true
	log.Printf("FFmpeg auto-install completed")
	return nil
}

func ensureHomeEnv() (homeDir string, source string, err error) {
	homeDir = strings.TrimSpace(os.Getenv("HOME"))
	if homeDir != "" {
		return homeDir, "env", nil
	}

	userHome, userHomeErr := os.UserHomeDir()
	userHome = strings.TrimSpace(userHome)
	if userHomeErr == nil && userHome != "" {
		if _, statErr := os.Stat(userHome); statErr == nil {
			if err := os.Setenv("HOME", userHome); err != nil {
				return "", "", err
			}
			return userHome, "user-home", nil
		}
	}

	homeDir = filepath.Join(os.TempDir(), "spotiflac-home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		return "", "", err
	}
	if err := os.Setenv("HOME", homeDir); err != nil {
		return "", "", err
	}

	return homeDir, "fallback", nil
}

func envBoolDefaultTrue(name string) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if raw == "" {
		return true
	}

	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func generateToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
