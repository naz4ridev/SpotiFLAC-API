package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Eyevinn/mp4ff/mp4"
)

// decryptAmazonMP4 CENC-decrypts a fragmented MP4 (as returned by the Amazon
// spotbye pool: an encrypted .mp4 + a "KID:KEY" hex key) into outPath, using the
// mp4ff library. Amazon returns a single key, so DecryptSegment with that key
// suffices (no multi-KID handling needed).
func decryptAmazonMP4(keySpec, inPath, outPath string) error {
	keyHex := strings.TrimSpace(keySpec)
	if i := strings.LastIndex(keyHex, ":"); i >= 0 { // "KID:KEY" -> KEY
		keyHex = strings.TrimSpace(keyHex[i+1:])
	}
	key, err := mp4.UnpackKey(keyHex)
	if err != nil {
		return fmt.Errorf("unpack key: %w", err)
	}

	in, err := os.Open(inPath)
	if err != nil {
		return err
	}
	defer in.Close()

	f, err := mp4.DecodeFile(in)
	if err != nil {
		return fmt.Errorf("decode mp4: %w", err)
	}
	if !f.IsFragmented() || f.Init == nil {
		return fmt.Errorf("encrypted mp4 is not fragmented/has no init segment")
	}

	decInfo, err := mp4.DecryptInit(f.Init)
	if err != nil {
		return fmt.Errorf("decrypt init: %w", err)
	}

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()

	if err := f.Init.Encode(out); err != nil {
		return err
	}
	for _, seg := range f.Segments {
		// Amazon uses "sparse" senc: only some fragments are encrypted, and
		// DecryptSegment errors on a fragment without a senc box. Decrypt per
		// fragment, skipping those that have no senc.
		for _, frag := range seg.Fragments {
			if !fragmentHasSenc(frag) {
				continue
			}
			if err := mp4.DecryptFragment(frag, decInfo, key); err != nil {
				return fmt.Errorf("decrypt fragment: %w", err)
			}
		}
		// sidx offsets become invalid after decryption; drop them.
		seg.Sidx = nil
		seg.Sidxs = nil
		if err := seg.Encode(out); err != nil {
			return err
		}
	}
	return nil
}

func fragmentHasSenc(frag *mp4.Fragment) bool {
	if frag == nil || frag.Moof == nil {
		return false
	}
	for _, traf := range frag.Moof.Trafs {
		if traf == nil {
			continue
		}
		if has, _ := traf.ContainsSencBox(); has {
			return true
		}
	}
	return false
}
