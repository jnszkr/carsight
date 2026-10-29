// Package buildinfo reports which carsight build is running, so drives can
// record the version that logged them.
package buildinfo

import "runtime/debug"

// Version is the module version Go stamped into the binary: a release tag
// (v0.1.3) for builds of a tagged commit, otherwise a pseudo-version that
// names the commit, or "dev" when the build recorded none.
func Version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// Line is the version followed by the commit it was built from, when the
// build recorded one.
func Line() string {
	line := "carsight " + Version()
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return line
	}
	vcs := map[string]string{}
	for _, s := range bi.Settings {
		vcs[s.Key] = s.Value
	}
	if rev := vcs["vcs.revision"]; len(rev) >= 7 {
		line += " (commit " + rev[:7]
		if t := vcs["vcs.time"]; t != "" {
			line += ", " + t
		}
		if vcs["vcs.modified"] == "true" {
			line += ", modified"
		}
		line += ")"
	}
	return line
}
