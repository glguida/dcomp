package proxy

import "golang.org/x/sys/unix"

func renameNoReplaceAtomic(oldPath, newPath string) error {
	return unix.RenamexNp(oldPath, newPath, unix.RENAME_EXCL)
}
