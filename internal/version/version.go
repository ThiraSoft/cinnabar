// Package version porte les métadonnées de build injectées par -ldflags.
package version

var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)
