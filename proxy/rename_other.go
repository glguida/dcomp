//go:build !linux && !darwin

package proxy

import "golang.org/x/sys/unix"

func renameNoReplaceAtomic(oldPath, newPath string) error {
	return unix.ENOSYS
}
