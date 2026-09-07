package main

import (
	"fmt"
	"path/filepath"

	"github.com/gofrs/flock"
)

func lockDataDir(dir string) (func(), error) {
	lock := flock.New(filepath.Join(dir, ".hoorific-instance.lock"))
	acquired, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock runtime data directory: %w", err)
	}
	if !acquired {
		return nil, fmt.Errorf("runtime data directory is already in use; replicas require separate data directories")
	}
	return func() { _ = lock.Unlock() }, nil
}
