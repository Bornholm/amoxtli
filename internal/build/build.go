// Package build carries version information injected at build time via
// -ldflags "-X github.com/bornholm/amoxtli/internal/build.Version=...".
package build

var (
	Version     = "dev"
	LongVersion = "dev"
)
