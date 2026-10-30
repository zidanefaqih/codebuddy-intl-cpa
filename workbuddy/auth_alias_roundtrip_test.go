package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func marshalAliasFixture(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func parseAliasFixture(t *testing.T, raw []byte) pluginapi.AuthData {
	t.Helper()
	body := marshalAliasFixture(t, pluginapi.AuthParseRequest{
		Provider: providerName, FileName: "workbuddy-alias-regression.json", RawJSON: raw,
	})
	out, err := handleParseAuth(body)
	if err != nil {
		t.Fatal(err)
	}
	response := decodeParseAuth(t, out)
	if !response.Handled {
		t.Fatal("workbuddy credential was not handled")
	}
	return response.Auth
}

func assertRoutingAliases(t *testing.T, ad pluginapi.AuthData, want any) {
	t.Helper()
	var got any
	if err := json.Unmarshal([]byte(ad.Attributes["model_aliases"]), &got); err != nil {
		t.Fatalf("runtime aliases missing or invalid: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime aliases = %#v, want %#v", got, want)
	}
}

// Exercise the same ParseAuth response consumed directly by host.auth.save.
// Checking StorageJSON alone misses aliases lost from the runtime attributes.
func TestAuthRoutingAliasesSurviveSaveAndRefresh(t *testing.T) {
	for _, realm := range []struct{ name, domain, model string }{
		{"cn", "www.codebuddy.cn", "cn/upstream-model"},
		{"global", "www.codebuddy.ai", "upstream-model"},
	} {
		for _, flat := range []bool{false, true} {
			for _, key := range []string{"model_aliases", "model-aliases"} {
				t.Run(realm.name+"/"+map[bool]string{true: "flat", false: "nested"}[flat]+"/"+key, func(t *testing.T) {
					for _, disabled := range []bool{false, true} {
						aliases := []any{
							map[string]any{"name": realm.model, "alias": "public-model", "fork": true, "force-mapping": true},
							map[string]any{"name": realm.model, "alias": "second-alias", "fork": false},
						}
						auth := map[string]any{"accessToken": "old-at", "refreshToken": "rt", "domain": realm.domain, "expiresAt": float64(9999999999)}
						doc := map[string]any{
							"type": providerName, key: aliases, "disabled": disabled,
							"weight": float64(8200), "priority": float64(0),
							"future_setting": map[string]any{"keep": true},
						}
						if flat {
							for k, value := range auth {
								doc[k] = value
							}
							doc["uid"] = "alias-regression"
						} else {
							doc["auth"] = auth
							doc["account"] = map[string]any{"uid": "alias-regression", "nickname": "nick"}
						}
						raw := marshalAliasFixture(t, doc)
						for save := 0; save < 3; save++ {
							sa, err := parseStored(raw)
							if err != nil {
								t.Fatal(err)
							}
							// A lifecycle note write retains the physical credential.
							raw, err = buildAuthFileJSONPreserving(&hostAuthPhysical{JSON: raw}, sa, disabled, "updated by lifecycle", nil)
							if err != nil {
								t.Fatal(err)
							}
							parsed := parseAliasFixture(t, raw)
							assertRoutingAliases(t, parsed, aliases)
							if parsed.Disabled != disabled || parsed.ID != "" || parsed.FileName != "workbuddy-alias-regression.json" {
								t.Fatal("parse changed disabled state or host-owned identity")
							}
							sa.Auth.AccessToken = "new-at"
							refreshed := toAuthDataForRefresh(sa, authMetadataFromJSON(raw), disabled, raw)
							assertRoutingAliases(t, refreshed, aliases)
							if refreshed.Disabled != disabled || refreshed.ID != "" || refreshed.FileName != "" {
								t.Fatal("refresh changed disabled state or host-owned identity")
							}
							for _, ad := range []pluginapi.AuthData{parsed, refreshed} {
								var stored map[string]any
								if err := json.Unmarshal(ad.StorageJSON, &stored); err != nil {
									t.Fatal(err)
								}
								for _, field := range []string{key, "weight", "priority", "future_setting"} {
									if !reflect.DeepEqual(stored[field], doc[field]) || !reflect.DeepEqual(ad.Metadata[field], doc[field]) {
										t.Fatalf("save %d: field %s lost from storage or metadata", save, field)
									}
								}
							}
							updated, err := parseStored(refreshed.StorageJSON)
							if err != nil {
								t.Fatal(err)
							}
							if updated.Auth.AccessToken != "new-at" {
								t.Fatal("alias export reverted refreshed tokens")
							}
							raw = refreshed.StorageJSON
						}
						// Updating and then clearing the persisted list must not retain
						// runtime aliases from a previous parse or refresh.
						if err := json.Unmarshal(raw, &doc); err != nil {
							t.Fatal(err)
						}
						replacement := []any{map[string]any{"name": realm.model, "alias": "replacement"}}
						doc[key] = replacement
						assertRoutingAliases(t, parseAliasFixture(t, marshalAliasFixture(t, doc)), replacement)
						for _, remove := range []bool{false, true} {
							doc[key] = []any{}
							if remove {
								delete(doc, key)
							}
							raw = marshalAliasFixture(t, doc)
							sa, err := parseStored(raw)
							if err != nil {
								t.Fatal(err)
							}
							for _, ad := range []pluginapi.AuthData{parseAliasFixture(t, raw), toAuthDataForRefresh(sa, authMetadataFromJSON(raw), disabled, raw)} {
								if _, exists := ad.Attributes["model_aliases"]; exists {
									t.Fatal("cleared aliases survived in runtime attributes")
								}
							}
						}
					}
				})
			}
		}
	}
}

func TestAuthRoutingAliasesCanonicalPrecedence(t *testing.T) {
	legacy := []any{map[string]any{"name": "upstream", "alias": "legacy"}}
	canonical := []any{map[string]any{"name": "upstream", "alias": "canonical"}}
	for _, value := range []any{canonical, nil, []any{}, "invalid", map[string]any{"alias": "invalid"}} {
		var doc map[string]any
		if err := json.Unmarshal(sampleNestedAuthJSON("alias-precedence"), &doc); err != nil {
			t.Fatal(err)
		}
		doc["model_aliases"], doc["model-aliases"] = value, legacy
		ad := parseAliasFixture(t, marshalAliasFixture(t, doc))
		if reflect.DeepEqual(value, canonical) {
			assertRoutingAliases(t, ad, canonical)
		} else if _, exists := ad.Attributes["model_aliases"]; exists {
			t.Fatal("empty or invalid canonical field must not fall back to legacy aliases")
		}
	}
}
