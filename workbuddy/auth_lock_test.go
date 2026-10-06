package main

import (
	"sync"
	"testing"
	"time"
)

func TestLockAuthMutationSerializesEquivalentIdentities(t *testing.T) {
	first := lockAuthMutation("runtime-auth", "record-auth")

	acquired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		unlock := lockAuthMutation("record-auth", "runtime-auth")
		close(acquired)
		unlock()
		close(done)
	}()

	select {
	case <-acquired:
		t.Fatal("equivalent identity lock acquired while first mutation was held")
	case <-time.After(20 * time.Millisecond):
	}
	first()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("equivalent identity lock did not become available")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lock goroutine did not finish")
	}
}

func TestLockAuthMutationDoesNotDeadlockOnReverseMultiIdentityOrder(t *testing.T) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(reverse bool) {
			defer wg.Done()
			<-start
			if reverse {
				unlock := lockAuthMutation("b-auth", "a-auth")
				unlock()
				return
			}
			unlock := lockAuthMutation("a-auth", "b-auth")
			unlock()
		}(i == 1)
	}
	close(start)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reverse-order lock acquisition deadlocked")
	}
}

func TestPruneAuthMutationLocksKeepsHeldEntries(t *testing.T) {
	unlock := lockAuthMutation("live-runtime", "live-record")
	pruneAuthMutationLocks(map[string]struct{}{})
	authMutationLocks.Lock()
	_, runtimePresent := authMutationLocks.byKey["live-runtime"]
	_, recordPresent := authMutationLocks.byKey["live-record"]
	authMutationLocks.Unlock()
	if !runtimePresent || !recordPresent {
		t.Fatal("pruning removed a held mutation lock")
	}
	unlock()
	pruneAuthMutationLocks(map[string]struct{}{})
	authMutationLocks.Lock()
	_, runtimePresent = authMutationLocks.byKey["live-runtime"]
	_, recordPresent = authMutationLocks.byKey["live-record"]
	authMutationLocks.Unlock()
	if runtimePresent || recordPresent {
		t.Fatal("idle mutation locks were not pruned")
	}
}
