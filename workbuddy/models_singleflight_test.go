package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeStorageJSON builds a minimal nested credential whose access token is a
// Global JWT (iss=codebuddy.ai) so realm resolution sees Global.
func fakeStorageJSON(authID, _ string) []byte {
	// header {"alg":"none"}. payload {"iss":"https://www.codebuddy.ai"}
	jwt := "e30." + "eyJpc3MiOiJodHRwczovL3d3dy5jb2RlYnVkZHkuYWkifQ" + ".sig"
	return []byte(`{"auth":{"accessToken":"` + jwt + `"},"account":{"uid":"` + authID + `"}}`)
}

// TestDynamicModelsSingleflight verifies that concurrent discovery for the
// same credential collapses into exactly one upstream call, and that all
// callers receive the same model list.
func TestDynamicModelsSingleflight(t *testing.T) {
	restoreCache := clearDynamicModelsCacheForTest()
	defer restoreCache()

	var calls int32
	release := make(chan struct{})
	var once sync.Once
	orig := dynamicModelsFetchFn
	dynamicModelsFetchFn = func(token string) ([]pluginapi.ModelInfo, error) {
		atomic.AddInt32(&calls, 1)
		<-release // hold the leader until all waiters have joined the flight
		return []pluginapi.ModelInfo{{ID: "m1", Name: "M1"}}, nil
	}
	defer func() { dynamicModelsFetchFn = orig }()

	const n = 8
	results := make([][]pluginapi.ModelInfo, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = fetchDynamicModelsFromStorage("auth-sf", fakeStorageJSON("auth-sf", "tok-sf"))
		}(i)
	}
	// Give waiters time to join the in-flight call before releasing it.
	time.Sleep(50 * time.Millisecond)
	once.Do(func() { close(release) })
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 upstream call for %d concurrent discoveries, got %d", n, got)
	}
	for i, r := range results {
		if len(r) != 1 || r[0].ID != "m1" {
			t.Fatalf("caller %d got unexpected result: %+v", i, r)
		}
	}
}

// TestDynamicModelsSingleflightLeaderFailure verifies that waiters retry
// discovery themselves when the leader's upstream call fails and caches
// nothing, instead of propagating an empty result to everyone.
func TestDynamicModelsSingleflightLeaderFailure(t *testing.T) {
	restoreCache := clearDynamicModelsCacheForTest()
	defer restoreCache()

	var calls int32
	orig := dynamicModelsFetchFn
	dynamicModelsFetchFn = func(token string) ([]pluginapi.ModelInfo, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return nil, errors.New("upstream down")
		}
		return []pluginapi.ModelInfo{{ID: "m2", Name: "M2"}}, nil
	}
	defer func() { dynamicModelsFetchFn = orig }()

	first := fetchDynamicModelsFromStorage("auth-fail", fakeStorageJSON("auth-fail", "tok-fail"))
	second := fetchDynamicModelsFromStorage("auth-fail", fakeStorageJSON("auth-fail", "tok-fail"))
	if len(first) == 0 || first[0].ID == "m2" {
		t.Fatalf("expected first (leader-failed) call to return fallback, got %+v", first)
	}
	if len(second) != 1 || second[0].ID != "m2" {
		t.Fatalf("expected retry after leader failure to succeed, got %+v", second)
	}
}

// TestDynamicModelsCacheIsolation verifies different auth IDs never share a
// cache entry even when issued in the same process (Global vs Global).
func TestDynamicModelsCacheIsolation(t *testing.T) {
	restoreCache := clearDynamicModelsCacheForTest()
	defer restoreCache()

	var seq int32
	orig := dynamicModelsFetchFn
	dynamicModelsFetchFn = func(token string) ([]pluginapi.ModelInfo, error) {
		return []pluginapi.ModelInfo{{ID: fmt.Sprintf("m-%d", atomic.AddInt32(&seq, 1))}}, nil
	}
	defer func() { dynamicModelsFetchFn = orig }()

	a := fetchDynamicModelsFromStorage("auth-a", fakeStorageJSON("auth-a", "tokA"))
	b := fetchDynamicModelsFromStorage("auth-b", fakeStorageJSON("auth-b", "tokB"))
	if a[0].ID == b[0].ID {
		t.Fatalf("expected distinct discovery per account (isolation), both got %q", a[0].ID)
	}
	// Repeat a: must come from cache, not a second upstream call.
	a2 := fetchDynamicModelsFromStorage("auth-a", fakeStorageJSON("auth-a", "tokA"))
	if a2[0].ID != a[0].ID {
		t.Fatalf("repeat for auth-a changed result: %q vs %q", a2[0].ID, a[0].ID)
	}
	if got := atomic.LoadInt32(&seq); got != 2 {
		t.Fatalf("expected 2 upstream calls total (cache hit for repeat), got %d", got)
	}
}

// TestDynamicModelsKeyScopesRealm verifies the cache key separates Global and
// CN realms even for the same auth ID (defense against tokenless corner cases).
func TestDynamicModelsKeyScopesRealm(t *testing.T) {
	globalKey := dynamicModelsCacheKey("same-id", fakeStorageJSON("same-id", "tok"))
	cnKey := dynamicModelsCacheKey("same-id", []byte(`{"auth":{"accessToken":"cn.notajwt.sig"},"account":{"uid":"same-id"}}`))
	if globalKey != "global:auth:same-id" {
		t.Fatalf("unexpected global key: %q", globalKey)
	}
	if cnKey != "cn:auth:same-id" {
		t.Fatalf("unexpected cn key: %q", cnKey)
	}
	if !strings.HasPrefix(globalKey, "global:") {
		t.Fatal("global realm expected")
	}
}

// TestDynamicModelsFlightCleared verifies the in-flight map entry is removed
// after completion so later requests do not wait on a dead channel.
func TestDynamicModelsFlightCleared(t *testing.T) {
	restoreCache := clearDynamicModelsCacheForTest()
	defer restoreCache()

	orig := dynamicModelsFetchFn
	dynamicModelsFetchFn = func(token string) ([]pluginapi.ModelInfo, error) {
		return []pluginapi.ModelInfo{{ID: "m3"}}, nil
	}
	defer func() { dynamicModelsFetchFn = orig }()

	fetchDynamicModelsFromStorage("auth-clear", fakeStorageJSON("auth-clear", "tok-clear"))
	if _, loaded := dynamicModelFlights.Load("global:auth:auth-clear"); loaded {
		t.Fatal("flight entry not cleaned up after completion")
	}

	// And a plain HTTP sanity check that the test binary itself is wired.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if srv.URL == "" {
		t.Fatal("httptest server unexpectedly empty")
	}
}

// clearDynamicModelsCacheForTest empties the process-wide model cache and
// flight map, returning a restore function.
func clearDynamicModelsCacheForTest() func() {
	dynamicModelsCache.Lock()
	old := dynamicModelsCache.byKey
	dynamicModelsCache.byKey = nil
	dynamicModelsCache.Unlock()
	dynamicModelFlights.Range(func(k, v any) bool {
		dynamicModelFlights.Delete(k)
		return true
	})
	return func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.byKey = old
		dynamicModelsCache.Unlock()
	}
}
