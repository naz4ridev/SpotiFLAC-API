// Package c2config is the persistent, hot-reloadable store for the C2 endpoints
// and tunable settings the API talks to (the spotbye download hosts, the lyric
// providers, the status source, the monochrome instance lists, ...).
//
// Historically these lived hard-coded in the process environment (.env). They
// change between SpotiFLAC-Next releases, so they are now kept in a small SQLite
// database that can be inspected and edited at runtime through the /admin CRUD
// API and web UI, and bulk-refreshed from a freshly extracted c2-manifest.json
// (see scripts/extract-spotiflac-next.py).
//
// The database is the source of truth; the environment is only used to seed an
// empty database on first boot. A SQLite-free build is kept possible by using
// the pure-Go modernc.org/sqlite driver (no cgo), matching the existing
// cgo-less Docker build.
package c2config

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Endpoint is a single C2 address row: e.g. service=qobuz, role=download,
// variant=x, url=https://qbzalt.spotbye.qzz.io.
type Endpoint struct {
	ID        int64     `json:"id"`
	Service   string    `json:"service"`
	Role      string    `json:"role"`
	Variant   string    `json:"variant"`
	URL       string    `json:"url"`
	Enabled   bool      `json:"enabled"`
	Priority  int       `json:"priority"`
	Notes     string    `json:"notes,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store wraps the SQLite database plus an in-memory cache for hot reads.
type Store struct {
	db *sql.DB

	mu        sync.RWMutex
	endpoints []Endpoint
	settings  map[string]string
}

// Open opens (creating if needed) the SQLite database at path, applies the
// schema, and loads the in-memory cache.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	db.SetMaxOpenConns(1) // single-writer; avoids SQLITE_BUSY under concurrency
	s := &Store{db: db, settings: map[string]string{}}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.Reload(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS c2_endpoints (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  service    TEXT    NOT NULL,
  role       TEXT    NOT NULL,
  variant    TEXT    NOT NULL DEFAULT '',
  url        TEXT    NOT NULL,
  enabled    INTEGER NOT NULL DEFAULT 1,
  priority   INTEGER NOT NULL DEFAULT 0,
  notes      TEXT    NOT NULL DEFAULT '',
  updated_at TEXT    NOT NULL,
  UNIQUE(service, role, variant)
);
CREATE TABLE IF NOT EXISTS settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL
);`
	_, err := s.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Reload refreshes the in-memory cache from the database.
func (s *Store) Reload() error {
	rows, err := s.db.Query(`SELECT id, service, role, variant, url, enabled, priority, notes, updated_at
		FROM c2_endpoints ORDER BY service, role, priority DESC, variant`)
	if err != nil {
		return fmt.Errorf("reload endpoints: %w", err)
	}
	defer rows.Close()

	var eps []Endpoint
	for rows.Next() {
		var e Endpoint
		var enabled int
		var updated string
		if err := rows.Scan(&e.ID, &e.Service, &e.Role, &e.Variant, &e.URL, &enabled, &e.Priority, &e.Notes, &updated); err != nil {
			return fmt.Errorf("scan endpoint: %w", err)
		}
		e.Enabled = enabled != 0
		e.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
		eps = append(eps, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	srows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return fmt.Errorf("reload settings: %w", err)
	}
	defer srows.Close()
	settings := map[string]string{}
	for srows.Next() {
		var k, v string
		if err := srows.Scan(&k, &v); err != nil {
			return fmt.Errorf("scan setting: %w", err)
		}
		settings[k] = v
	}
	if err := srows.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	s.endpoints = eps
	s.settings = settings
	s.mu.Unlock()
	return nil
}

// --- read accessors (served from cache) -------------------------------------

// Endpoints returns a copy of all endpoints.
func (s *Store) Endpoints() []Endpoint {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Endpoint, len(s.endpoints))
	copy(out, s.endpoints)
	return out
}

// EnabledURLs returns the URLs for a (service, role), enabled only, ordered by
// priority desc. Used by the download engines to pick active C2 addresses.
func (s *Store) EnabledURLs(service, role string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, e := range s.endpoints {
		if e.Service == service && e.Role == role && e.Enabled {
			out = append(out, e.URL)
		}
	}
	return out
}

// Setting returns a setting value, or fallback if unset/empty.
func (s *Store) Setting(key, fallback string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.settings[key]; ok && strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

// SettingCSV splits a comma-separated setting, trimming blanks; fallback if unset.
func (s *Store) SettingCSV(key string, fallback []string) []string {
	raw := s.Setting(key, "")
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

// Settings returns a copy of all settings.
func (s *Store) Settings() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.settings))
	for k, v := range s.settings {
		out[k] = v
	}
	return out
}

// --- writes ------------------------------------------------------------------

// UpsertEndpoint inserts or updates an endpoint keyed by (service, role, variant)
// and refreshes the cache. Returns the row id.
func (s *Store) UpsertEndpoint(e Endpoint) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	enabled := 0
	if e.Enabled {
		enabled = 1
	}
	res, err := s.db.Exec(`
		INSERT INTO c2_endpoints(service, role, variant, url, enabled, priority, notes, updated_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(service, role, variant) DO UPDATE SET
			url=excluded.url, enabled=excluded.enabled, priority=excluded.priority,
			notes=excluded.notes, updated_at=excluded.updated_at`,
		e.Service, e.Role, e.Variant, e.URL, enabled, e.Priority, e.Notes, now)
	if err != nil {
		return 0, fmt.Errorf("upsert endpoint: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, s.Reload()
}

// UpdateEndpointByID updates an existing endpoint addressed by its id.
func (s *Store) UpdateEndpointByID(e Endpoint) error {
	now := time.Now().UTC().Format(time.RFC3339)
	enabled := 0
	if e.Enabled {
		enabled = 1
	}
	res, err := s.db.Exec(`UPDATE c2_endpoints
		SET service=?, role=?, variant=?, url=?, enabled=?, priority=?, notes=?, updated_at=?
		WHERE id=?`,
		e.Service, e.Role, e.Variant, e.URL, enabled, e.Priority, e.Notes, now, e.ID)
	if err != nil {
		return fmt.Errorf("update endpoint %d: %w", e.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("endpoint %d not found", e.ID)
	}
	return s.Reload()
}

// DeleteEndpoint removes an endpoint by id.
func (s *Store) DeleteEndpoint(id int64) error {
	res, err := s.db.Exec(`DELETE FROM c2_endpoints WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete endpoint %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("endpoint %d not found", id)
	}
	return s.Reload()
}

// SetSetting upserts a single setting.
func (s *Store) SetSetting(key, value string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`INSERT INTO settings(key, value, updated_at) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, now)
	if err != nil {
		return fmt.Errorf("set setting %q: %w", key, err)
	}
	return s.Reload()
}

// IsEmpty reports whether the store has no endpoints and no settings (fresh DB).
func (s *Store) IsEmpty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.endpoints) == 0 && len(s.settings) == 0
}

// --- seeding & manifest import ----------------------------------------------

// Seed carries initial values for an empty database (typically from the
// environment so existing deployments keep working).
type Seed struct {
	Endpoints []Endpoint
	Settings  map[string]string
}

// SeedIfEmpty populates the database from seed only when it is empty, so it
// never clobbers operator edits on subsequent boots.
func (s *Store) SeedIfEmpty(seed Seed) (bool, error) {
	if !s.IsEmpty() {
		return false, nil
	}
	for _, e := range seed.Endpoints {
		if e.URL == "" {
			continue
		}
		if _, err := s.UpsertEndpoint(e); err != nil {
			return false, err
		}
	}
	for k, v := range seed.Settings {
		if v == "" {
			continue
		}
		if err := s.SetSetting(k, v); err != nil {
			return false, err
		}
	}
	return true, s.Reload()
}

// manifest mirrors the JSON emitted by scripts/extract-spotiflac-next.py.
type manifest struct {
	Endpoints map[string]struct {
		Service    string `json:"service"`
		Role       string `json:"role"`
		Host       string `json:"host"`
		ExampleURL string `json:"example_url"`
	} `json:"endpoints"`
	PublicProviders map[string]string `json:"public_providers"`
}

// ImportManifest bulk-applies a c2-manifest.json. Endpoints become c2_endpoints
// rows (variant ""); public providers and the status source become settings.
// Existing rows are updated in place; nothing is deleted. Returns counts.
// ImportManifest REPLACES the C2 endpoint set with the manifest's: every existing
// c2_endpoints row is deleted and only the new version's endpoints are inserted,
// so the list never accumulates stale hosts across releases. Operator settings
// (API keys, sp_dc cookie, Monochrome lists, ...) are preserved — only the
// version-derived settings (endpoint.* providers, status.source_url) are upserted.
// The endpoint replacement is transactional: on any error nothing changes.
func (s *Store) ImportManifest(raw []byte) (endpoints, settings int, err error) {
	var m manifest
	if err = json.Unmarshal(raw, &m); err != nil {
		return 0, 0, fmt.Errorf("parse manifest: %w", err)
	}

	// Deterministic order for predictable logs.
	keys := make([]string, 0, len(m.Endpoints))
	for k := range m.Endpoints {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Discard the previous version's endpoints entirely.
	if _, err = tx.Exec(`DELETE FROM c2_endpoints`); err != nil {
		return 0, 0, fmt.Errorf("clear endpoints: %w", err)
	}
	for _, k := range keys {
		ep := m.Endpoints[k]
		if ep.ExampleURL == "" {
			continue
		}
		if _, err = tx.Exec(`INSERT INTO c2_endpoints(service, role, variant, url, enabled, priority, notes, updated_at)
			VALUES(?,?,?,?,1,0,'',?)`, ep.Service, ep.Role, "", ep.ExampleURL, now); err != nil {
			return 0, 0, fmt.Errorf("insert endpoint %s: %w", k, err)
		}
		endpoints++
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit: %w", err)
	}

	// Version-derived settings are upserted (operator settings are untouched).
	for name, url := range m.PublicProviders {
		if url == "" {
			continue
		}
		if err = s.SetSetting("endpoint."+name, url); err != nil {
			return endpoints, settings, err
		}
		settings++
	}
	if st, ok := m.Endpoints["_status.status"]; ok && st.ExampleURL != "" {
		if err = s.SetSetting("status.source_url", st.ExampleURL); err != nil {
			return endpoints, settings, err
		}
		settings++
	}
	if err = s.Reload(); err != nil {
		return endpoints, settings, err
	}
	return endpoints, settings, nil
}
