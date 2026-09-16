// Package hostfs preserves private or group-shared host state permissions.
// Setgid state directories carry the sharing policy to new descendants.
package hostfs

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// Group reports the group of a directory explicitly configured for sharing.
func Group(path string) (int, bool) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) && filepath.Dir(path) != path {
		return Group(filepath.Dir(path))
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSetgid == 0 || info.Mode().Perm()&0070 != 0070 {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Gid), true
}

func RestrictDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	mode := os.FileMode(0700)
	if _, shared := Group(path); shared {
		mode = 0770 | os.ModeSetgid
	}
	if info.Mode()&(os.ModePerm|os.ModeSetgid) == mode {
		return nil
	}
	return os.Chmod(path, mode)
}

// Mode gives group members the same access as the owner under shared parents.
func Mode(path string, mode os.FileMode) os.FileMode {
	if _, shared := Group(filepath.Dir(path)); shared {
		mode |= (mode.Perm() & 0700) >> 3
	}
	return mode
}

func Mkdir(path string, mode os.FileMode) error {
	mode = Mode(path, mode)
	if _, shared := Group(filepath.Dir(path)); shared {
		mode |= os.ModeSetgid
	}
	if err := os.Mkdir(path, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode) // An arbitrary caller umask must not remove group access.
}

func MkdirAll(path string, mode os.FileMode) error {
	path = filepath.Clean(path)
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", path)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := MkdirAll(filepath.Dir(path), mode); err != nil {
		return err
	}
	if err := Mkdir(path, mode); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

// OpenFile sets permissions only on newly created files, never another user's file.
func OpenFile(path string, flags int, mode os.FileMode) (*os.File, error) {
	if flags&os.O_CREATE == 0 {
		return os.OpenFile(path, flags, mode)
	}
	mode = Mode(path, mode)
	file, err := os.OpenFile(path, flags|os.O_EXCL, mode)
	if errors.Is(err, os.ErrExist) && flags&os.O_EXCL == 0 {
		return os.OpenFile(path, flags&^os.O_CREATE, mode)
	}
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// Init creates a root or uses an existing root without changing its contents
// or sharing policy. An empty directory can be initialized in place.
func Init(path, group string) (string, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	gid := -1
	if group != "" {
		record, err := user.LookupGroup(group)
		if err != nil {
			return "", fmt.Errorf("group %q: %w", group, err)
		}
		gid, err = strconv.Atoi(record.Gid)
		if err != nil {
			return "", err
		}
		groups, err := os.Getgroups()
		if err != nil {
			return "", err
		}
		member := os.Geteuid() == 0 || os.Getegid() == gid
		for _, candidate := range groups {
			member = member || candidate == gid
		}
		if !member {
			return "", fmt.Errorf("current user is not a member of group %q", group)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err == nil && len(entries) != 0 {
		if gid >= 0 {
			existing, shared := Group(root)
			if !shared || existing != gid {
				return "", fmt.Errorf("%s is already initialized with different permissions", root)
			}
		}
		return filepath.EvalSymlinks(root)
	}
	var missing []string
	for path := root; gid >= 0; path = filepath.Dir(path) {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			break
		}
		missing = append(missing, path)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	mode := os.FileMode(0700)
	if gid >= 0 {
		for i := len(missing) - 1; i >= 0; i-- {
			if err := os.Chown(missing[i], -1, gid); err != nil {
				return "", err
			}
			if err := os.Chmod(missing[i], 0770|os.ModeSetgid); err != nil {
				return "", err
			}
		}
		if err := os.Chown(root, -1, gid); err != nil {
			return "", err
		}
		mode = 0770 | os.ModeSetgid
	}
	if err := os.Chmod(root, mode); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(root)
}

// SocketDirectory prepares an auxiliary socket directory with the runtime's
// sharing policy, including when a long socket path has to use an ancestor.
func SocketDirectory(path, runtime string) error {
	if gid, shared := Group(runtime); shared {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		err := os.Mkdir(path, 0700)
		if err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err == nil {
			if err := os.Chown(path, -1, gid); err != nil {
				return err
			}
			return os.Chmod(path, 0770|os.ModeSetgid)
		}
		actual, ok := Group(path)
		if !ok || actual != gid {
			return fmt.Errorf("socket directory %s has different group permissions", path)
		}
		return nil
	}
	return MkdirAll(path, 0700)
}
