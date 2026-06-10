package main

import "testing"

func TestSpotbyePrefixAndQuality(t *testing.T) {
	cases := map[string]string{"tidal": "tdl", "qobuz": "qbz", "amazon": "amz", "deezer": "dzr", "apple": "am", "bogus": ""}
	for svc, want := range cases {
		if got := spotbyePrefix(svc); got != want {
			t.Errorf("spotbyePrefix(%q) = %q, want %q", svc, got, want)
		}
	}
	// Quality tokens accepted by /api/dl per service.
	if spotbyeQuality("deezer") != "320" || spotbyeQuality("qobuz") != "24" {
		t.Errorf("unexpected quality: deezer=%s qobuz=%s", spotbyeQuality("deezer"), spotbyeQuality("qobuz"))
	}
}

func TestPickQobuzTrack(t *testing.T) {
	items := []qobuzTrack{
		{ID: 1, ISRC: "AAA", Hires: false, MaximumBitDepth: 16},
		{ID: 2, ISRC: "BBB", Hires: true, MaximumBitDepth: 24},
		{ID: 3, ISRC: "CCC", Hires: false, MaximumBitDepth: 16},
	}
	// Exact ISRC match wins.
	if got := pickQobuzTrack(items, "ccc"); got != 3 {
		t.Errorf("ISRC match: got %d want 3", got)
	}
	// No ISRC match -> highest quality (hires/bit depth).
	if got := pickQobuzTrack(items, "ZZZ"); got != 2 {
		t.Errorf("quality pick: got %d want 2 (hires)", got)
	}
}
