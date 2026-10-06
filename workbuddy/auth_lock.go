package main

import (
	"sort"
	"strings"
	"sync"
)

type authMutationLockEntry struct {
	mu   sync.Mutex
	refs int
}

var authMutationLocks struct {
	sync.Mutex
	byKey map[string]*authMutationLockEntry
}

// lockAuthMutation serializes all plugin-side mutations for one account. CPA
// exposes two identities for the same credential: auth_index is used by the
// host auth callbacks, while auth.ID is used by the core scheduler/provider
// refresh path. Locking both in sorted order prevents stale token/metadata
// saves when those identities differ and avoids lock-order deadlocks.
func lockAuthMutation(authIndex, authID string) func() {
	return lockAuthMutationKeys(authIndex, authID)
}

// lockAuthMutationKeys is the multi-identity form used when an operation can
// address an auth by more than the runtime index and core record ID (for
// example, import can race through both canonical and legacy filenames).
func lockAuthMutationKeys(identities ...string) func() {
	keys := mutationLockKeys(identities...)
	if len(keys) == 0 {
		return func() {}
	}

	entries := make([]*authMutationLockEntry, len(keys))
	authMutationLocks.Lock()
	if authMutationLocks.byKey == nil {
		authMutationLocks.byKey = make(map[string]*authMutationLockEntry)
	}
	for i, key := range keys {
		entry := authMutationLocks.byKey[key]
		if entry == nil {
			entry = &authMutationLockEntry{}
			authMutationLocks.byKey[key] = entry
		}
		entry.refs++
		entries[i] = entry
	}
	authMutationLocks.Unlock()

	for _, entry := range entries {
		entry.mu.Lock()
	}
	return func() {
		for i := len(entries) - 1; i >= 0; i-- {
			entries[i].mu.Unlock()
		}
		authMutationLocks.Lock()
		for i, key := range keys {
			entry := entries[i]
			entry.refs--
			if entry.refs == 0 && authMutationLocks.byKey[key] == entry {
				delete(authMutationLocks.byKey, key)
			}
		}
		authMutationLocks.Unlock()
	}
}

func mutationLockKeys(identities ...string) []string {
	set := make(map[string]struct{}, len(identities))
	for _, key := range identities {
		if key = strings.TrimSpace(key); key != "" {
			set[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// pruneAuthMutationLocks removes only idle entries for accounts no longer
// known to the host. Held or awaited entries have refs > 0 and remain in the
// registry, so pruning can never split one account across two mutexes.
func pruneAuthMutationLocks(live map[string]struct{}) {
	authMutationLocks.Lock()
	defer authMutationLocks.Unlock()
	for key, entry := range authMutationLocks.byKey {
		if entry.refs == 0 {
			if _, ok := live[key]; !ok {
				delete(authMutationLocks.byKey, key)
			}
		}
	}
}
