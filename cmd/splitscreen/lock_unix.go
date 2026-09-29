//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockConfig takes an exclusive advisory lock beside the config file and
// returns the release function.
//
// Every edit is read-modify-write, so two concurrent invocations — a control
// plane registering two boxes at once, say — would otherwise both read the same
// file and the second rename would silently drop the first edit. The lock is a
// sibling file rather than the config itself because the config is replaced by
// rename, and a lock on an inode that has just been renamed away protects
// nothing.
func lockConfig(path string) (func(), error) {
	f, err := os.OpenFile(path+".lock", os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	deadline := time.Now().Add(configLockTimeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("lock %s: another edit has held it for %s", path, configLockTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// preserveOwner gives a replacement file the original's owner. Edits typically
// run as root (sudo, or SSM Run Command) against a file the gateway reads as
// its own unprivileged user; without this the rename would leave a root-owned
// 0640 config the gateway can no longer read, and the next reload would fail.
func preserveOwner(f *os.File, orig os.FileInfo) error {
	st, ok := orig.Sys().(*syscall.Stat_t)
	if !ok || os.Geteuid() != 0 {
		return nil
	}
	return f.Chown(int(st.Uid), int(st.Gid))
}
