package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const unifiedCredential = `{"type":"workbuddy","channel":"trae","label":"kept","prefix":"team","priority":4,"custom":9007199254740993,"disabled_models":["hidden"],"auth":{"accessToken":"fixture","refreshToken":"fixture-refresh","expiresAt":4102444800,"deviceId":"1234567890123456","machineId":"machine","domain":"trae.cn","unknown":"retained"},"account":{"uid":"fixture-user","nickname":"Fixture"}}`

type unifiedFixture struct {
	stored []byte
	calls  []string
	saves  int
}

func newUnifiedFixture(t *testing.T) *unifiedFixture {
	t.Helper()
	previousHost := unifiedHostCall
	traeRuntime.Lock()
	previousEngine := traeRuntime.engine
	traeRuntime.engine = nil
	traeRuntime.Unlock()
	f := &unifiedFixture{stored: []byte(unifiedCredential)}
	unifiedHostCall = f.host
	t.Cleanup(func() {
		traeRuntime.Lock()
		if traeRuntime.engine != nil {
			traeRuntime.engine.Stop()
		}
		traeRuntime.engine = previousEngine
		traeRuntime.Unlock()
		unifiedHostCall = previousHost
	})
	return f
}

func (f *unifiedFixture) host(method string, raw []byte) ([]byte, error) {
	f.calls = append(f.calls, method)
	switch method {
	case pluginabi.MethodHostAuthList:
		return okEnvelope(rpcHostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{
			{Name: "workbuddy-cb.json", AuthIndex: "cb", Provider: providerName},
			{Name: "trae-fixture-user.json", AuthIndex: "tr", Provider: providerName},
			{Name: "trae-legacy.json", AuthIndex: "legacy", Provider: "trae"},
		}})
	case pluginabi.MethodHostAuthGet:
		var r pluginapi.HostAuthGetRequest
		_ = json.Unmarshal(raw, &r)
		if r.AuthIndex == "cb" {
			return okEnvelope(pluginapi.HostAuthGetResponse{Name: "workbuddy-cb.json", JSON: []byte(`{"type":"workbuddy","access_token":"cb-fixture"}`)})
		}
		return okEnvelope(pluginapi.HostAuthGetResponse{Name: "trae-fixture-user.json", JSON: f.stored})
	case pluginabi.MethodHostAuthSave:
		var r pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		if r.Name != "trae-fixture-user.json" {
			return nil, fmt.Errorf("unexpected save target")
		}
		f.stored = r.JSON
		f.saves++
		return okEnvelope(nil)
	case pluginabi.MethodHostHTTPOperationOpen:
		return okEnvelope(map[string]string{"operation_id": "fixture"})
	case pluginabi.MethodHostHTTPDo:
		var r struct {
			Request struct {
				URL string `json:"url"`
			} `json:"request"`
		}
		_ = json.Unmarshal(raw, &r)
		body := `{"config_info_list":[{"config_name":"chosen","display_config":{"display_name":"Chosen"}},{"config_name":"hidden"}]}`
		if strings.Contains(r.Request.URL, "ExchangeToken") {
			body = `{"Result":{"Token":"fresh-fixture","RefreshToken":"rotated-fixture","TokenExpireDuration":7200}}`
		}
		return okEnvelope(pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)})
	default:
		return nil, fmt.Errorf("unexpected callback %s", method)
	}
}

func unifiedResult(t *testing.T, method string, req, target any) {
	t.Helper()
	raw, handled, err := dispatchTrae(method, mustJSON(req))
	if err != nil || !handled {
		t.Fatalf("dispatch %s handled=%v err=%v", method, handled, err)
	}
	payload, err := hostBridgeUnwrap(raw, method)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(payload, target); err != nil {
		t.Fatal(err)
	}
}

func TestUnifiedTraeParsePreservesSettingsAndIdentity(t *testing.T) {
	newUnifiedFixture(t)
	var result pluginapi.AuthParseResponse
	unifiedResult(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{Provider: providerName, FileName: "trae-fixture-user.json", RawJSON: []byte(unifiedCredential)}, &result)
	if !result.Handled || result.Auth.Provider != providerName || result.Auth.ID != "" || result.Auth.Prefix != "team" || result.Auth.Attributes["channel"] != "trae" {
		t.Fatal("outer provider identity or routing lost")
	}
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(result.Auth.StorageJSON, &doc)
	if string(doc["custom"]) != "9007199254740993" || string(doc["disabled_models"]) != `["hidden"]` || string(doc["channel"]) != `"trae"` {
		t.Fatal("settings or numeric precision lost")
	}
}

func TestUnifiedTraePoolAndCredentialBoundary(t *testing.T) {
	f := newUnifiedFixture(t)
	raw, err := traeHostCall(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := hostBridgeUnwrap(raw, pluginabi.MethodHostAuthList)
	var list rpcHostAuthListResponse
	_ = json.Unmarshal(payload, &list)
	if len(list.Files) != 1 || list.Files[0].AuthIndex != "tr" || list.Files[0].Provider != "trae" {
		t.Fatal("Trae pool admitted CodeBuddy or legacy plugin accounts")
	}
	if _, err = traePhysical("cb"); err == nil {
		t.Fatal("Trae management accepted CodeBuddy account")
	}
	for _, name := range []string{"workbuddy-cb.json", "trae-../bad.json", `trae-..\bad.json`} {
		if _, err = traeHostCall(pluginabi.MethodHostAuthSave, mustJSON(pluginapi.HostAuthSaveRequest{Name: name, JSON: f.stored})); err == nil {
			t.Fatal("invalid write accepted")
		}
	}
	for _, tc := range []struct {
		model   string
		storage []byte
	}{{"chosen", f.stored}, {"trae/chosen", []byte(`{"type":"workbuddy","access_token":"cb"}`)}} {
		_, handled, e := dispatchTrae(pluginabi.MethodExecutorExecute, mustJSON(pluginapi.ExecutorRequest{Model: tc.model, StorageJSON: tc.storage}))
		if !handled || e == nil {
			t.Fatal("cross-channel request accepted")
		}
	}
}

func TestUnifiedTraeModelsArePrefixedAndPerAccount(t *testing.T) {
	f := newUnifiedFixture(t)
	var result pluginapi.ModelResponse
	request := pluginapi.AuthModelRequest{AuthID: "tr", AuthProvider: providerName, StorageJSON: f.stored}
	unifiedResult(t, pluginabi.MethodModelForAuth, request, &result)
	if result.Provider != providerName || len(result.Models) != 1 || result.Models[0].ID != "trae/chosen" {
		t.Fatalf("unexpected model IDs: %+v", result.Models)
	}
	// The cached catalog must not be mutated or reduced by the previous account's exclusions.
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(f.stored, &doc)
	delete(doc, "disabled_models")
	f.stored = mustJSON(doc)
	request.StorageJSON = f.stored
	unifiedResult(t, pluginabi.MethodModelForAuth, request, &result)
	if len(result.Models) != 2 || result.Models[0].ID != "trae/chosen" {
		t.Fatal("cached catalog polluted by prefixes or per-account settings")
	}
}

func TestUnifiedTraeRefreshAndSettingsPreserveEachOther(t *testing.T) {
	f := newUnifiedFixture(t)
	f.stored = []byte(strings.Replace(unifiedCredential, "4102444800", "1", 1))
	if err := unifiedTraeEngine().SetDisabledModels("tr", []string{"chosen"}); err != nil {
		t.Fatal(err)
	}
	var result pluginapi.AuthRefreshResponse
	unifiedResult(t, pluginabi.MethodAuthRefresh, pluginapi.AuthRefreshRequest{AuthID: "tr", AuthProvider: providerName, StorageJSON: f.stored}, &result)
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(f.stored, &doc)
	if string(doc["disabled_models"]) != `["chosen"]` || string(doc["custom"]) != "9007199254740993" || !strings.Contains(string(f.stored), "fresh-fixture") {
		t.Fatal("token rotation lost settings or did not persist")
	}
	if result.Auth.Provider != providerName || result.Auth.ID != "" || result.Auth.FileName != "" {
		t.Fatal("refresh changed host identity")
	}
	before := len(f.calls)
	_, handled, err := dispatchTrae(pluginabi.MethodExecutorExecute, mustJSON(pluginapi.ExecutorRequest{AuthID: "tr", AuthProvider: providerName, StorageJSON: f.stored, Model: "trae/chosen", Payload: []byte(`{"messages":[{"role":"user","content":"test"}]}`)}))
	if !handled || err == nil {
		t.Fatal("disabled model executed")
	}
	for _, call := range f.calls[before:] {
		if call == pluginabi.MethodHostHTTPDoStream {
			t.Fatal("disabled model reached upstream")
		}
	}
}

func TestUnifiedUsageJournalsKeepCreditTotalsSeparate(t *testing.T) {
	t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())
	oldCB, oldTR := usageStore, traeUsageStore
	usageStore = &requestUsageStore{perDay: map[string]map[string]*dayAgg{}}
	traeUsageStore = &requestUsageStore{logPrefix: "trae-request-usage", perDay: map[string]map[string]*dayAgg{}}
	t.Cleanup(func() { usageStore, traeUsageStore = oldCB, oldTR })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	usageStore.record(requestUsageRecord{Timestamp: now, AuthIndex: "cb", Credit: 2})
	traeUsageStore.record(requestUsageRecord{Timestamp: now, AuthIndex: "tr", Channel: "trae", Credit: 100})
	result := unifiedUsageQuery(pluginapi.ManagementRequest{})
	if len(result["records"].([]requestUsageRecord)) != 2 {
		t.Fatal("shared query lost a channel")
	}
	credit, _ := usageStore.allTimeTotals()
	if credit != 2 {
		t.Fatal("CodeBuddy totals included Trae credits")
	}
	trCredit, _ := traeUsageStore.allTimeTotals()
	if trCredit != 100 {
		t.Fatal("Trae journal did not load independently")
	}
}

func TestUnifiedTraeReimportKeepsModelSettingsAndDevice(t *testing.T) {
	f := newUnifiedFixture(t)
	request := pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/workbuddy/trae/import", Body: []byte(`{"type":"trae","auth":{"accessToken":"replacement-fixture","domain":"trae.cn"},"account":{"uid":"fixture-user"}}`)}
	raw, handled, err := handleUnifiedManagement(request, "/v0/management/plugins/workbuddy")
	if err != nil || !handled {
		t.Fatal(err)
	}
	payload, _ := hostBridgeUnwrap(raw, pluginabi.MethodManagementHandle)
	var response pluginapi.ManagementResponse
	_ = json.Unmarshal(payload, &response)
	if response.StatusCode != 200 {
		t.Fatalf("import failed: %s", response.Body)
	}
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(f.stored, &doc)
	if string(doc["disabled_models"]) != `["hidden"]` || string(doc["custom"]) != "9007199254740993" || !strings.Contains(string(f.stored), "replacement-fixture") || !strings.Contains(string(f.stored), `"machineId":"machine"`) {
		t.Fatal("re-import damaged settings, token or device identity")
	}
}
