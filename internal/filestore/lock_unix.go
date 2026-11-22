//go:build unix

package filestore

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive flock on path, creating it if needed. The lock
// belongs to this open file, so it also excludes other goroutines and
// processes that open the same path.
func lockFile(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil // closing releases the lock
}
