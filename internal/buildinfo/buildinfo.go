// Package buildinfo reports the version of this module as built into the
// running binary, for the client identification the SDK packages send.
package buildinfo

import (
	"runtime/debug"
	"strings"
	"sync"
)

// module is the path of this module.
const module = "github.com/ironpark/gelati"

// devVersion is reported when the module version is unknown, as in a
// checkout of the module itself.
const devVersion = "0.0.0-dev"

// Version returns the module's version without its "v" prefix, or
// "0.0.0-dev" when it is unknown. The build info is read once.
var Version = sync.OnceValue(version)

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return devVersion
	}
	mod := &info.Main
	for _, dep := range info.Deps {
		if dep.Path == module {
			mod = dep
			break
		}
	}
	if mod.Path != module || mod.Version == "" || mod.Version == "(devel)" {
		return devVersion
	}
	return strings.TrimPrefix(mod.Version, "v")
}
