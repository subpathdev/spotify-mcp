//go:build unix

package filestore

import (
	"os"
	"syscall"
)

// flock takes an exclusive flock on a lock file, creating it if needed. The
// lock belongs to the open file, so it also excludes other goroutines and
// processes that open the same path.
type flock struct{ path string }

func newLocker(path string) locker { return flock{path: path} }

func (l flock) lock() (release func(), err error) {
	f, err := os.OpenFile(l.path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil // closing releases the lock
}
