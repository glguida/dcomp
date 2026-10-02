package proxy

import "golang.org/x/sys/unix"

func renameNoReplaceAtomic(oldPath, newPath string) error {
	return unix.Renameat2(
		unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_NOREPLACE,
	)
}
