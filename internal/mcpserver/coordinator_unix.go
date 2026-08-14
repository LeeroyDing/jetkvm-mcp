//go:build darwin || linux

package mcpserver

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const lockPollInterval = 25 * time.Millisecond

type fileCoordinator struct {
	// dir is deliberately retained for the coordinator lifetime. Opening the
	// lock with openat(2) through this validated descriptor prevents a later
	// path traversal from being redirected to a different lock inode.
	dir  *os.File
	name string
	path string // diagnostics-free test visibility only; never used to open
}

func (c *fileCoordinator) close() error {
	if c == nil || c.dir == nil {
		return nil
	}
	return boundedLockError(c.dir.Close())
}

func newFileCoordinator(baseURL string) (*fileCoordinator, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("mcpserver: user cache directory unavailable for session coordination")
	}
	return newFileCoordinatorAt(baseURL, filepath.Join(cacheDir, "jetkvm-mcp", "session-locks-v1"))
}

func newFileCoordinatorAt(baseURL, lockDir string) (*fileCoordinator, error) {
	identity, err := canonicalDeviceIdentity(baseURL)
	if err != nil {
		return nil, fmt.Errorf("mcpserver: %w", err)
	}
	dir, err := openPrivateDirectory(lockDir)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%x.lock", sha256.Sum256([]byte(identity)))
	return &fileCoordinator{
		dir:  dir,
		name: name,
		path: filepath.Join(lockDir, name),
	}, nil
}

// openPrivateDirectory walks from the filesystem root using no-follow
// directory descriptors. Every existing ancestor must be owned by root or
// the current user and must not be writable by another user. The one
// conventional exception is a root-owned sticky directory such as /tmp;
// sticky ownership then protects the current user's private child. Newly
// created components are 0700. Retaining the final descriptor closes the
// validation/use race and lets lock() use openat(2) without re-traversal.
func openPrivateDirectory(path string) (*os.File, error) {
	absPath, err := filepath.Abs(path)
	if err != nil || !filepath.IsAbs(absPath) {
		return nil, fmt.Errorf("mcpserver: secure session-lock directory unavailable")
	}
	absPath = filepath.Clean(absPath)
	parts := strings.Split(strings.TrimPrefix(absPath, string(filepath.Separator)), string(filepath.Separator))
	if len(parts) == 0 || (len(parts) == 1 && parts[0] == "") {
		return nil, fmt.Errorf("mcpserver: refusing filesystem root as session-lock directory")
	}

	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("mcpserver: secure session-lock directory unavailable")
	}
	current := os.NewFile(uintptr(fd), "session-lock-root")
	if current == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("mcpserver: secure session-lock directory unavailable")
	}

	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			_ = current.Close()
			return nil, fmt.Errorf("mcpserver: invalid session-lock directory")
		}

		childFD, openErr := openOrCreateDirectoryAt(int(current.Fd()), part)
		if openErr != nil {
			_ = current.Close()
			return nil, fmt.Errorf("mcpserver: opening secure session-lock directory: %w", boundedLockError(openErr))
		}
		child := os.NewFile(uintptr(childFD), "session-lock-directory")
		if child == nil {
			_ = unix.Close(childFD)
			_ = current.Close()
			return nil, fmt.Errorf("mcpserver: secure session-lock directory unavailable")
		}

		var stat unix.Stat_t
		statErr := unix.Fstat(childFD, &stat)
		if statErr == nil {
			statErr = validateDirectoryStat(&stat, i == len(parts)-1)
		}
		_ = current.Close()
		if statErr != nil {
			_ = child.Close()
			return nil, statErr
		}
		current = child
	}

	return current, nil
}

func openOrCreateDirectoryAt(parentFD int, name string) (int, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	for {
		fd, err := unix.Openat(parentFD, name, flags, 0)
		switch {
		case err == nil:
			return fd, nil
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ENOENT):
			err = unix.Mkdirat(parentFD, name, 0o700)
			if err == nil || errors.Is(err, unix.EEXIST) {
				continue
			}
			return -1, err
		default:
			return -1, err
		}
	}
}

func validateDirectoryStat(stat *unix.Stat_t, final bool) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("mcpserver: session-lock path contains a non-directory")
	}
	euid := uint32(os.Geteuid())
	if stat.Uid != 0 && stat.Uid != euid {
		return fmt.Errorf("mcpserver: session-lock path is not owned by root or the current user")
	}
	permissions := uint32(stat.Mode) & 0o777
	otherWritable := permissions&0o022 != 0
	rootSticky := stat.Uid == 0 && uint32(stat.Mode)&unix.S_ISVTX != 0
	if otherWritable && !rootSticky {
		return fmt.Errorf("mcpserver: session-lock path is writable by another user")
	}
	if final {
		if stat.Uid != euid {
			return fmt.Errorf("mcpserver: session-lock directory is not owned by the current user")
		}
		if permissions != 0o700 {
			return fmt.Errorf("mcpserver: session-lock directory permissions must be private (0700)")
		}
	}
	return nil
}

func (c *fileCoordinator) lock(ctx context.Context) (func() error, error) {
	flags := unix.O_CREAT | unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	fd, err := unix.Openat(int(c.dir.Fd()), c.name, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("mcpserver: opening secure device session lock: %w", boundedLockError(err))
	}
	f := os.NewFile(uintptr(fd), "device-session.lock")
	if f == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("mcpserver: opening secure device session lock: local filesystem error")
	}
	closeOnError := func(err error) (func() error, error) {
		_ = f.Close()
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		return closeOnError(fmt.Errorf("mcpserver: inspecting device session lock: %w", boundedLockError(err)))
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return closeOnError(fmt.Errorf("mcpserver: device session lock must be a private regular file (0600)"))
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return closeOnError(fmt.Errorf("mcpserver: device session lock is not owned by the current user"))
	}
	if stat.Nlink != 1 {
		return closeOnError(fmt.Errorf("mcpserver: hard-linked device session locks are not permitted"))
	}

	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		switch {
		case err == nil:
			var once sync.Once
			var releaseErr error
			return func() error {
				once.Do(func() {
					releaseErr = errors.Join(
						boundedLockError(unix.Flock(int(f.Fd()), unix.LOCK_UN)),
						boundedLockError(f.Close()),
					)
				})
				return releaseErr
			}, nil
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN):
			timer := time.NewTimer(lockPollInterval)
			select {
			case <-timer.C:
				continue
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return closeOnError(fmt.Errorf("mcpserver: waiting for exclusive device session: %w", ctx.Err()))
			}
		default:
			return closeOnError(fmt.Errorf("mcpserver: acquiring device session lock: %w", boundedLockError(err)))
		}
	}
}

// boundedLockError keeps filesystem paths out of user-facing errors. The
// category is sufficient to diagnose permissions/type/contention and cannot
// reflect a credential-bearing environment-supplied cache path.
func boundedLockError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, fs.ErrPermission):
		return errors.New("permission denied")
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("path unavailable")
	case errors.Is(err, unix.ELOOP):
		return errors.New("symbolic links are not permitted")
	default:
		return errors.New("local filesystem error")
	}
}
