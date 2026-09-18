package update

import "fmt"

// localForkBuild marks this binary as a local fork build: it carries patches
// (ci_mode) that upstream releases lack, so replacing it with an upstream
// release would silently restore forge check waiting. Self-update refuses and
// the "new version available" banner stays quiet; rebuild from the fork's
// local branch instead.
const localForkBuild = true

func errLocalForkUpdate(version string) error {
	return fmt.Errorf("no-mistakes update is disabled: %s is a local fork build; rebuild and install from the fork's local branch instead", version)
}
