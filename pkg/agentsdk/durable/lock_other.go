//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package durable

import "sync"

// Platforms without flock retain in-process serialization. The CAS document
// still protects revisions, but callers needing cross-process ownership should
// use PostgresStore on these platforms.
func withFileLock(path string, fn func() error) error {
	value, _ := filesystemLocks.LoadOrStore(path, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()
	return fn()
}
