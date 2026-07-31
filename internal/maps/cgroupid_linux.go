//go:build linux

package maps

import (
	"os"
	"syscall"
)

// dirCgroupID returns the cgroup ID of a cgroup v2 directory: the
// kernfs inode number, which is exactly what the BPF scheduler reads
// via cgrp->kn->id. Declared as a variable so tests can stub it.
var dirCgroupID = func(path string) (uint64, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Ino, true
}
