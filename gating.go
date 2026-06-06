package main

import (
	"context"
	"net/http"

	"spotiflacapi/internal/status"
)

// handleStatus exposes the aggregated availability of all worlds: spotiflac
// (upstream), spotiflac-next (the status payload), and monochrome. Clients can
// use it to decide which services are worth attempting.
func (s *apiServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorResponse{OK: false, Error: "method not allowed"})
		return
	}
	if s.status == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{OK: false, Error: "status tracker not initialized"})
		return
	}
	snap := s.status.Get(r.Context())
	writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
		*status.Snapshot
	}{OK: true, Snapshot: snap})
}

// filterActiveServices drops services that the status payload reports as fully
// down, so we only attempt active methods. It never returns an empty list: if
// gating would remove every service (e.g. the status source is unreachable) it
// falls back to the original order rather than blocking all downloads.
func (s *apiServer) filterActiveServices(ctx context.Context, order []string) []string {
	if !s.enforceActive || s.status == nil {
		return order
	}
	var active []string
	for _, svc := range order {
		if s.status.ServiceActive(ctx, svc) {
			active = append(active, svc)
		}
	}
	if len(active) == 0 {
		return order
	}
	return active
}
