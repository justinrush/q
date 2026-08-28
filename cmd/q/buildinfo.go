// Build identity for the running binary.
//
// The version is stamped by install.sh through -ldflags. When it is not — a
// plain `go build`, or `go run` — the value falls back to the module's own build
// information, so `q --version` says something true either way.
package main

import "runtime/debug"

// buildVersion is the release identifier, set at link time with
// -ldflags "-X main.buildVersion=v1.2.3".
var buildVersion = ""

// buildCommit is the git revision, set the same way.
var buildCommit = ""

// semanticVersion returns the version string, resolving it from the embedded
// build info when it was not stamped.
func semanticVersion() string {
	if buildVersion != "" {
		return buildVersion
	}

	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "dev"
	}

	return info.Main.Version
}

// buildRevision returns the git revision, falling back to the VCS stamp the Go
// toolchain embeds for builds made inside a checkout.
func buildRevision() string {
	if buildCommit != "" {
		return buildCommit
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

// versionString renders version and revision for `q --version`.
func versionString() string {
	if rev := buildRevision(); rev != "" {
		if len(rev) > 12 {
			rev = rev[:12]
		}

		return semanticVersion() + " (" + rev + ")"
	}

	return semanticVersion()
}
