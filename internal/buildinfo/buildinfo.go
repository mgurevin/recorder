// Package buildinfo resolves the version shared by recorder executables.
package buildinfo

import "runtime/debug"

// injectedVersion is set by the release build with -ldflags. Keeping the
// override here gives future binaries the same version contract without
// duplicating build metadata logic.
var injectedVersion string

// Version returns the injected release version, the Go module version used by
// go install, or "dev" for an unversioned source build.
func Version() string {
	if injectedVersion != "" {
		return injectedVersion
	}

	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "dev"
	}

	return info.Main.Version
}
