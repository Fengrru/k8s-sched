//go:build !linux

package maps

// dirCgroupID is a stub on non-Linux platforms (development only):
// cgroup IDs cannot be resolved, so callers fall back to PID entries.
// Declared as a variable so tests can stub it.
var dirCgroupID = func(string) (uint64, bool) {
	return 0, false
}
