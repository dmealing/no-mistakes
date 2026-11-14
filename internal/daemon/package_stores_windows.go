//go:build windows

package daemon

import (
	"path/filepath"
	"strings"
)

// sameFilesystem reports whether a and b are on one volume, the condition for
// a hardlink between them. A path without a volume name is not comparable.
func sameFilesystem(a, b string) bool {
	av, bv := filepath.VolumeName(a), filepath.VolumeName(b)
	return av != "" && strings.EqualFold(av, bv)
}
