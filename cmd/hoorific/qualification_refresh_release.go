//go:build !qualification

package main

import (
	"hoorific/internal/credential"
	"hoorific/internal/store"
)

// qualificationRefreshRepository deliberately has no qualification controls in
// ordinary builds. The release binary delegates refresh persistence directly to
// the configured durable store.
func qualificationRefreshRepository(db *store.Store) (credential.Repository, error) {
	return db, nil
}
