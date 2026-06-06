package main

import "testing"

func TestApplyIDTemplate(t *testing.T) {
	cases := []struct {
		endpoint, id, want string
	}{
		{"https://dzr.spotbye.qzz.io/api/track/%d?f=flac", "123", "https://dzr.spotbye.qzz.io/api/track/123?f=flac"},
		{"https://jmdl.spotbye.qzz.io/%s?format_id=5", "abc", "https://jmdl.spotbye.qzz.io/abc?format_id=5"},
		{"https://qbz.spotbye.qzz.io/api/download-music?track_id=", "999", "https://qbz.spotbye.qzz.io/api/download-music?track_id=999"},
		{"https://qbzalt.spotbye.qzz.io/", "42", "https://qbzalt.spotbye.qzz.io/42"},
		{"https://example.com/fixed", "7", "https://example.com/fixed"},
	}
	for _, c := range cases {
		if got := applyIDTemplate(c.endpoint, c.id); got != c.want {
			t.Errorf("applyIDTemplate(%q,%q) = %q, want %q", c.endpoint, c.id, got, c.want)
		}
	}
}

func TestFirstEnabledURL(t *testing.T) {
	if got := firstEnabledURL(nil, "fb"); got != "fb" {
		t.Errorf("empty list should return fallback, got %q", got)
	}
	if got := firstEnabledURL([]string{"  ", "https://a"}, "fb"); got != "https://a" {
		t.Errorf("should skip blanks, got %q", got)
	}
	if got := firstEnabledURL([]string{"https://x"}, "fb"); got != "https://x" {
		t.Errorf("got %q", got)
	}
}
