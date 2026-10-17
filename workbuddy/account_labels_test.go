package main

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestAccountLabelsUseImportedNicknameWithoutChangingIDs(t *testing.T) {
	files := []pluginapi.HostAuthFileEntry{
		{ID: "workbuddy-new-uid.json", AuthIndex: "new-index", Label: "old host name [Global]"},
		{ID: "workbuddy-peer-uid.json", AuthIndex: "peer-index", Label: "peer [Global]"},
		{ID: "workbuddy-cn-uid.json", AuthIndex: "cn-index", Label: "CN name [CN]"},
		{ID: "fallback-id", AuthIndex: "fallback-index", Label: "fallback [Global]"},
	}
	creds := map[string]*storedAuth{
		"new-index":      {Auth: storedTokens{Domain: "https://www.codebuddy.ai"}, Account: storedAccount{Nickname: "Intl-new-import"}},
		"peer-index":     {Auth: storedTokens{Domain: "https://www.codebuddy.ai"}, Account: storedAccount{Nickname: "Intl-new-import"}},
		"cn-index":       {Auth: storedTokens{Domain: "https://copilot.tencent.com"}, Account: storedAccount{Nickname: "CN-import"}},
		"fallback-index": {Auth: storedTokens{Domain: "https://www.codebuddy.ai"}},
	}
	stubAuthSources(t, files, creds)
	labels := accountLabelsForRealm(realmGlobal)
	if len(labels) != 3 || labels[files[0].ID] != "Intl-new-import" || labels[files[1].ID] != "Intl-new-import" || labels["fallback-id"] != "fallback" {
		t.Fatalf("labels=%v", labels)
	}
	if cn := accountLabelsForRealm(realmCN); len(cn) != 1 || cn[files[2].ID] != "CN-import" {
		t.Fatalf("CN labels=%v", cn)
	}
	// A subsequent import/update is visible without any account-name mapping.
	creds["new-index"].Account.Nickname = "Intl-renamed-import"
	if got := accountLabelsForRealm(realmGlobal)[files[0].ID]; got != "Intl-renamed-import" {
		t.Fatalf("updated label=%s", got)
	}
}
