package main

import (
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"testing"
)

func TestRateScopeUsesUpstreamAliasNotClientModel(t *testing.T) {
	t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())
	cs := exhaustedCandidates(t)
	cs[0].RateModel = "gpt-6.1-sol"
	stubAuthSources(t, []pluginapi.HostAuthFileEntry{testAuthEntry("exhaust-a", "record-a", false), testAuthEntry("exhaust-b", "record-b", false)}, map[string]*storedAuth{"exhaust-a": cs[0].Auth, "exhaust-b": cs[1].Auth})
	markCandidateUnavailable(cs[0], outcomeRateLimited, "")
	pool, _, err := buildRequestFailoverPool("alias-sol", "exhaust-a", storageJSONForAuth(cs[0].Auth), map[string]string{"model_alias": `{"alias-sol":"gpt-6.1-sol"}`})
	if err != nil {
		t.Fatal(err)
	}
	got := pool.ordered(nil)
	if len(got) != 1 || got[0].AuthID != "exhaust-b" || got[0].RateModel != "gpt-6.1-sol" {
		t.Fatalf("alias bypassed cooldown: %+v", got)
	}
}
