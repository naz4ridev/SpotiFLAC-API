package main

import (
	"runtime/debug"
	"sync"
)

// spotiflacModuleVersion reports the pinned version of the upstream SpotiFLAC
// module baked into this binary (e.g. v0.0.0-20260608230652-954cfe9d4fac). It is
// exposed via /health so the updater can verify that a NEW build was actually
// deployed before running the post-deploy smoke test (otherwise the smoke would
// validate the still-running old container).
var (
	moduleVersionOnce sync.Once
	moduleVersion     string
)

func spotiflacModuleVersion() string {
	moduleVersionOnce.Do(func() {
		moduleVersion = "unknown"
		info, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		for _, dep := range info.Deps {
			if dep.Path == "github.com/afkarxyz/SpotiFLAC" {
				moduleVersion = dep.Version
				return
			}
		}
	})
	return moduleVersion
}
