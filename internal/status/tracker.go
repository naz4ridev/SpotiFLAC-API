// Package status reports which download/lyric/metadata methods are currently
// usable, mirroring SpotiFLAC-Next's behaviour: the app fetches a "downloader
// status" payload (a gist / JSON document) listing each backend variant as
// "up" or "down" and only attempts the active ones.
//
// The Tracker aggregates three worlds:
//   - spotiflac-next : the status payload (per service + variant up/down).
//   - monochrome     : live reachability of the configured instances.
//   - spotiflac      : the upstream Go provider + a Spotify reachability probe.
//
// Results are cached for a short TTL so /v1/status and the download gating path
// don't hammer the upstreams on every request.
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ServiceStatus is the normalized per-service view of the status payload:
// which variants exist and whether each is up, plus a convenience Active flag.
type ServiceStatus struct {
	Variants map[string]bool `json:"variants"`
	Active   bool            `json:"active"`
}

// NextStatus maps service name -> ServiceStatus (e.g. "tidal", "qobuz", "apple").
type NextStatus map[string]ServiceStatus

// MonochromeStatus summarizes reachability of the monochrome instances.
type MonochromeStatus struct {
	InstancesUp    int            `json:"instances_up"`
	InstancesTotal int            `json:"instances_total"`
	Details        map[string]bool `json:"details,omitempty"`
}

// UpstreamStatus is the spotiflac (upstream Go provider) view.
type UpstreamStatus struct {
	Available       bool `json:"available"`
	SpotifyReachable bool `json:"spotify_reachable"`
}

// Snapshot is the full aggregated status at a point in time.
type Snapshot struct {
	CheckedAt  time.Time         `json:"checked_at"`
	Next       NextStatus        `json:"spotiflac_next"`
	Monochrome MonochromeStatus  `json:"monochrome"`
	Upstream   UpstreamStatus    `json:"spotiflac_upstream"`
	RawNext    map[string]string `json:"spotiflac_next_raw,omitempty"`
	Errors     map[string]string `json:"errors,omitempty"`
}

// Config carries the runtime knobs and dependency hooks for the Tracker.
type Config struct {
	HTTPClient *http.Client
	TTL        time.Duration

	// StatusSourceURL returns the URL of the downloader-status payload.
	StatusSourceURL func() string
	// MonochromeInstances returns the instance URLs to probe.
	MonochromeInstances func() []string
}

// Tracker fetches and caches the aggregated status.
type Tracker struct {
	cfg Config

	mu       sync.Mutex
	cached   *Snapshot
	cachedAt time.Time
}

func New(cfg Config) *Tracker {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 60 * time.Second
	}
	return &Tracker{cfg: cfg}
}

// Get returns a cached snapshot, refreshing it if older than the TTL.
func (t *Tracker) Get(ctx context.Context) *Snapshot {
	t.mu.Lock()
	if t.cached != nil && time.Since(t.cachedAt) < t.cfg.TTL {
		snap := t.cached
		t.mu.Unlock()
		return snap
	}
	t.mu.Unlock()

	snap := t.refresh(ctx)

	t.mu.Lock()
	t.cached = snap
	t.cachedAt = time.Now()
	t.mu.Unlock()
	return snap
}

// ServiceActive reports whether a service (tidal/qobuz/amazon/...) has at least
// one "up" variant in the latest status. Unknown services default to active so
// gating never blocks a service we have no information about.
func (t *Tracker) ServiceActive(ctx context.Context, service string) bool {
	snap := t.Get(ctx)
	if snap == nil || snap.Next == nil {
		return true
	}
	st, ok := snap.Next[strings.ToLower(service)]
	if !ok {
		return true
	}
	return st.Active
}

// ActiveVariants returns the variant keys reported "up" for a service, ordered
// a..e then x (then any others), e.g. ["a","b","c","d","e","x"]. Used to build
// the live SpotiFLAC-Next Shared/Community pool hosts ({svc}-{variant}...).
func (t *Tracker) ActiveVariants(ctx context.Context, service string) []string {
	snap := t.Get(ctx)
	if snap == nil || snap.Next == nil {
		return nil
	}
	st, ok := snap.Next[strings.ToLower(service)]
	if !ok {
		return nil
	}
	rank := func(v string) int {
		switch v {
		case "a":
			return 0
		case "b":
			return 1
		case "c":
			return 2
		case "d":
			return 3
		case "e":
			return 4
		case "x":
			return 5
		default:
			return 6
		}
	}
	var up []string
	for v, isUp := range st.Variants {
		if isUp {
			up = append(up, v)
		}
	}
	sort.Slice(up, func(i, j int) bool { return rank(up[i]) < rank(up[j]) })
	return up
}

func (t *Tracker) refresh(ctx context.Context) *Snapshot {
	snap := &Snapshot{
		CheckedAt: time.Now().UTC(),
		Errors:    map[string]string{},
	}

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		next, raw, err := t.fetchNextStatus(ctx)
		if err != nil {
			snap.Errors["spotiflac_next"] = err.Error()
			return
		}
		snap.Next = next
		snap.RawNext = raw
	}()

	go func() {
		defer wg.Done()
		snap.Monochrome = t.probeMonochrome(ctx)
	}()

	go func() {
		defer wg.Done()
		snap.Upstream = t.probeUpstream(ctx)
	}()

	wg.Wait()
	if len(snap.Errors) == 0 {
		snap.Errors = nil
	}
	return snap
}

// fetchNextStatus pulls the status payload and normalizes its flat keys
// ("tidal_a":"up", "apple":"up") into service -> variant -> up.
func (t *Tracker) fetchNextStatus(ctx context.Context) (NextStatus, map[string]string, error) {
	if t.cfg.StatusSourceURL == nil {
		return nil, nil, fmt.Errorf("no status source configured")
	}
	src := strings.TrimSpace(t.cfg.StatusSourceURL())
	if src == "" {
		return nil, nil, fmt.Errorf("no status source configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := t.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("status source returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}

	var raw map[string]string
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, fmt.Errorf("parse status payload: %w", err)
	}
	return NormalizeNext(raw), raw, nil
}

// NormalizeNext converts a flat status payload into the structured view.
// Keys are "<service>_<variant>" (e.g. tidal_a) or bare "<service>" (apple).
func NormalizeNext(raw map[string]string) NextStatus {
	out := NextStatus{}
	for key, val := range raw {
		up := strings.EqualFold(strings.TrimSpace(val), "up")
		service, variant := key, ""
		if i := strings.LastIndex(key, "_"); i >= 0 {
			service, variant = key[:i], key[i+1:]
		}
		service = strings.ToLower(service)
		st, ok := out[service]
		if !ok {
			st = ServiceStatus{Variants: map[string]bool{}}
		}
		st.Variants[variant] = up
		if up {
			st.Active = true
		}
		out[service] = st
	}
	return out
}

// probeMonochrome concurrently checks instance reachability with a short timeout.
func (t *Tracker) probeMonochrome(ctx context.Context) MonochromeStatus {
	var instances []string
	if t.cfg.MonochromeInstances != nil {
		instances = t.cfg.MonochromeInstances()
	}
	out := MonochromeStatus{InstancesTotal: len(instances), Details: map[string]bool{}}
	if len(instances) == 0 {
		return out
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 4 * time.Second}
	for _, inst := range instances {
		wg.Add(1)
		go func(inst string) {
			defer wg.Done()
			ok := reachable(ctx, client, inst)
			mu.Lock()
			out.Details[inst] = ok
			if ok {
				out.InstancesUp++
			}
			mu.Unlock()
		}(inst)
	}
	wg.Wait()
	return out
}

func reachable(ctx context.Context, client *http.Client, rawURL string) bool {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		// HEAD may be unsupported; a GET to the root is a softer fallback.
		req, err = http.NewRequestWithContext(cctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return false
		}
		resp, err = client.Do(req)
		if err != nil {
			return false
		}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return resp.StatusCode < 500
}

func (t *Tracker) probeUpstream(ctx context.Context) UpstreamStatus {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(cctx, "tcp", "api.spotify.com:443")
	reachable := err == nil
	if conn != nil {
		_ = conn.Close()
	}
	return UpstreamStatus{Available: true, SpotifyReachable: reachable}
}

// SortedServices returns the service names of a snapshot in deterministic order.
func (s *Snapshot) SortedServices() []string {
	keys := make([]string, 0, len(s.Next))
	for k := range s.Next {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
