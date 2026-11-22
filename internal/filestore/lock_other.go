//go:build !unix

package filestore

// lockFile has no cross-process lock on this platform; the Store's mutex
// still protects one process.
func lockFile(string) (release func(), err error) { return func() {}, nil }
