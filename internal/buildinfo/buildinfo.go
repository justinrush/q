// Package buildinfo reports which build of q is running.
//
// The version is stamped by install.sh through -ldflags. When it is not — a
// plain `go build`, or `go run` — the value falls back to the module's own build
// information, so `q --version` says something true either way.
package buildinfo

import "runtime/debug"

// Version is the release identifier, set at link time with
// -ldflags "-X github.com/justinrush/q/internal/buildinfo.Version=v1.2.3".
var Version = ""

// Commit is the git revision, set the same way.
var Commit = ""

// Semantic returns the version string, resolving it from the embedded build
// info when it was not stamped.
func Semantic() string {
	if Version != "" {
		return Version
	}

	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "dev"
	}

	return info.Main.Version
}

// Revision returns the git revision, falling back to the VCS stamp the Go
// toolchain embeds for builds made inside a checkout.
func Revision() string {
	if Commit != "" {
		return Commit
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}

	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value
		}
	}

	return ""
}

// String renders version and revision for `q --version`.
func String() string {
	if rev := Revision(); rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}

		return Semantic() + " (" + rev + ")"
	}

	return Semantic()
}
