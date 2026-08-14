//go:build darwin || linux

package mcpserver

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// macOS spells its per-user temp directory through /var, a root-owned
// symlink to /private/var. Production lock paths do not follow symlinks by
// design; resolve only the test framework's already-created trusted temp
// root before appending paths that exercise our own no-follow checks.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestFileCoordinatorProcessHelper(t *testing.T) {
	mode := os.Getenv("JETKVM_LOCK_HELPER")
	if mode == "" {
		return
	}
	coord, err := newFileCoordinatorAt("http://jetkvm.local", os.Getenv("JETKVM_LOCK_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	if mode == "exit-held" {
		if _, err := coord.lock(context.Background()); err != nil {
			t.Fatal(err)
		}
		os.Exit(0) // model abrupt process termination without an explicit unlock
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := coord.lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("helper acquired parent-held cross-process lock: %v", err)
	}
}

func TestFileCoordinatorSerializesAcrossProcesses(t *testing.T) {
	dir := filepath.Join(resolvedTempDir(t), "process-locks")
	coord, err := newFileCoordinatorAt("http://jetkvm.local", dir)
	if err != nil {
		t.Fatal(err)
	}
	release, err := coord.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	cmd := exec.Command(os.Args[0], "-test.run=^TestFileCoordinatorProcessHelper$")
	cmd.Env = append(os.Environ(), "JETKVM_LOCK_HELPER=contend", "JETKVM_LOCK_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-process lock helper failed: %v\n%s", err, out)
	}
}

func TestFileCoordinatorLockIsReleasedWhenProcessExits(t *testing.T) {
	dir := filepath.Join(resolvedTempDir(t), "exit-locks")
	cmd := exec.Command(os.Args[0], "-test.run=^TestFileCoordinatorProcessHelper$")
	cmd.Env = append(os.Environ(), "JETKVM_LOCK_HELPER=exit-held", "JETKVM_LOCK_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("lock-exit helper failed: %v\n%s", err, out)
	}

	coord, err := newFileCoordinatorAt("http://jetkvm.local", dir)
	if err != nil {
		t.Fatal(err)
	}
	release, err := coord.lock(context.Background())
	if err != nil {
		t.Fatalf("OS did not release advisory lock when owner exited: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

func TestFileCoordinatorSerializesIndependentInstances(t *testing.T) {
	dir := filepath.Join(resolvedTempDir(t), "locks")
	first, err := newFileCoordinatorAt("http://jetkvm.local", dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newFileCoordinatorAt("http://JETKVM.local:80/", dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.path != second.path {
		t.Fatalf("URL aliases did not map to one lock: %q != %q", first.path, second.path)
	}

	releaseFirst, err := first.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if _, err := second.lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second coordinator acquired an already-held lock: %v", err)
	}
	if err := releaseFirst(); err != nil {
		t.Fatal(err)
	}

	releaseSecond, err := second.lock(context.Background())
	if err != nil {
		t.Fatalf("lock was not recoverable after release: %v", err)
	}
	if err := releaseSecond(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(first.path); err != nil {
		t.Fatalf("stable lock file was removed; unlinking permits split-inode locks: %v", err)
	}
}

func TestFileCoordinatorRejectsInsecureLockDirectory(t *testing.T) {
	dir := filepath.Join(resolvedTempDir(t), "shared-locks")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := newFileCoordinatorAt("http://jetkvm.local", dir); err == nil {
		t.Fatal("accepted a session-lock directory accessible to other users")
	}
}

func TestFileCoordinatorRejectsWritableIntermediateDirectory(t *testing.T) {
	parent := filepath.Join(resolvedTempDir(t), "shared-parent-LOCK-PATH-CANARY")
	if err := os.Mkdir(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	_, err := newFileCoordinatorAt("http://jetkvm.local", filepath.Join(parent, "private", "locks"))
	if err == nil {
		t.Fatal("accepted a lock path below an attacker-writable intermediate directory")
	}
	if strings.Contains(err.Error(), "LOCK-PATH-CANARY") {
		t.Fatalf("intermediate-directory error leaked its path: %v", err)
	}
}

func TestFileCoordinatorRejectsIntermediateSymlink(t *testing.T) {
	root := resolvedTempDir(t)
	target := filepath.Join(resolvedTempDir(t), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "redirect-LOCK-PATH-CANARY")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := newFileCoordinatorAt("http://jetkvm.local", filepath.Join(link, "locks"))
	if err == nil {
		t.Fatal("followed an intermediate symlink in the session-lock path")
	}
	if strings.Contains(err.Error(), "LOCK-PATH-CANARY") || strings.Contains(err.Error(), target) {
		t.Fatalf("intermediate-symlink error leaked a filesystem path: %v", err)
	}
}

func TestFileCoordinatorRejectsSymlinkLockFileWithoutLeakingPath(t *testing.T) {
	dir := filepath.Join(resolvedTempDir(t), "locks-LOCK-PATH-CANARY")
	coord, err := newFileCoordinatorAt("http://jetkvm.local", dir)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(resolvedTempDir(t), "target")
	if err := os.WriteFile(target, []byte("do not lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, coord.path); err != nil {
		t.Fatal(err)
	}
	_, err = coord.lock(context.Background())
	if err == nil {
		t.Fatal("followed a symlink in the device session lock path")
	}
	if strings.Contains(err.Error(), "LOCK-PATH-CANARY") || strings.Contains(err.Error(), target) {
		t.Fatalf("lock error leaked a filesystem path: %v", err)
	}
}
