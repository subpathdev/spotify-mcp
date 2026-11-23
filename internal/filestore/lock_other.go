//go:build !unix

package filestore

// noLock is the locker on platforms without flock: there is no cross-process
// lock, and the store's mutex still protects one process.
type noLock struct{}

func newLocker(string) locker { return noLock{} }

func (noLock) lock() (release func(), err error) { return func() {}, nil }
