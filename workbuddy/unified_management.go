package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

//go:embed unified_panel.html
var unifiedPanelHTML []byte

func unifiedManagementRegistration() managementRegistrationResponse {
	reg := managementRegistration()
	base := "/plugins/" + pluginRouteID
	for _, route := range [][2]string{
		{"GET", "/combined/accounts"}, {"GET", "/combined/usage"},
		{"GET", "/trae/accounts"}, {"GET", "/trae/usage"}, {"GET", "/trae/settings"},
		{"POST", "/trae/import"}, {"POST", "/trae/toggle"}, {"POST", "/trae/checkin"}, {"POST", "/trae/refresh"},
		{"POST", "/trae/login/start"}, {"POST", "/trae/login/poll"}, {"POST", "/trae/login/complete"},
		{"GET", "/trae/models"}, {"POST", "/trae/models/config"},
	} {
		reg.Routes = append(reg.Routes, managementRoute{Method: route[0], Path: base + route[1]})
	}
	reg.Resources = append(reg.Resources, resourceRoute{Path: "/codebuddy"})
	return reg
}

func handleUnifiedManagement(req pluginapi.ManagementRequest, base string) ([]byte, bool, error) {
	sub := strings.TrimPrefix(req.Path, base)
	if sub == "/combined/accounts" && req.Method == http.MethodGet {
		codebuddy := buildDashboardEx(false, false)
		child, err := traeManagementResult(req, "/accounts")
		if err != nil {
			child = map[string]any{"accounts": []any{}, "error": redactSecrets(err.Error())}
		}
		out, e := okEnvelope(mgmtJSONResponse(200, map[string]any{"codebuddy": codebuddy, "trae": child, "version": version}))
		return out, true, e
	}
	if sub == "/combined/usage" && req.Method == http.MethodGet {
		out, e := okEnvelope(mgmtJSONResponse(200, unifiedUsageQuery(req)))
		return out, true, e
	}
	if sub == "/update/check" && req.Method == http.MethodGet && strings.Contains(version, "+unified-trae.") {
		// This integration is maintained independently. An official CodeBuddy
		// binary would remove the embedded Trae engine.
		out, e := okEnvelope(mgmtJSONResponse(200, updateCheckResponse{Current: version, Latest: version, Available: false, Changelog: "Unified local build; update with the unified installer."}))
		return out, true, e
	}
	if !strings.HasPrefix(sub, "/trae/") {
		return nil, false, nil
	}
	if sub == "/trae/models" && req.Method == http.MethodGet {
		out, err := traeModelsForPanel(req)
		status := 200
		if err != nil {
			status = 400
			out = map[string]any{"error": redactSecrets(err.Error())}
		}
		response, e := okEnvelope(mgmtJSONResponse(status, out))
		return response, true, e
	}
	if sub == "/trae/models/config" && req.Method == http.MethodPost {
		out, err := saveTraeModelSettings(req)
		status := 200
		if err != nil {
			status = 400
			out = map[string]any{"error": redactSecrets(err.Error())}
		}
		response, e := okEnvelope(mgmtJSONResponse(status, out))
		return response, true, e
	}
	// Reuse Trae's login/import/balance/check-in operations within the one
	// authenticated workbuddy namespace. Only registered exact routes enter it.
	child := req
	child.Path = "/v0/management/plugins/trae" + strings.TrimPrefix(sub, "/trae")
	childRaw, err := unifiedTraeEngine().Call(pluginabi.MethodManagementHandle, mustJSON(child))
	if err != nil {
		return nil, true, err
	}
	if sub == "/trae/login/poll" {
		// Login already persisted the account. The browser only needs status,
		// never AuthData (which includes access and refresh tokens).
		payload, e := hostBridgeUnwrap(childRaw, pluginabi.MethodManagementHandle)
		if e != nil {
			return nil, true, e
		}
		var response pluginapi.ManagementResponse
		if e = json.Unmarshal(payload, &response); e != nil {
			return nil, true, e
		}
		var body map[string]json.RawMessage
		if e = json.Unmarshal(response.Body, &body); e != nil {
			return nil, true, e
		}
		delete(body, "Auth")
		delete(body, "auth")
		response.Body = mustJSON(body)
		out, e := okEnvelope(response)
		return out, true, e
	}
	return childRaw, true, nil
}

func unifiedUsageQuery(req pluginapi.ManagementRequest) map[string]any {
	limit, _ := strconv.Atoi(req.Query.Get("limit"))
	if limit < 1 {
		limit = 100
	}
	if limit > usageLimitMax {
		limit = usageLimitMax
	}
	usageStore.loadFromDisk()
	traeUsageStore.loadFromDisk()
	index, from, to := req.Query.Get("auth_index"), req.Query.Get("from"), req.Query.Get("to")
	records := append(usageStore.queryRecords(index, from, to, limit), traeUsageStore.queryRecords(index, from, to, limit)...)
	sort.SliceStable(records, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, records[i].Timestamp)
		b, _ := time.Parse(time.RFC3339Nano, records[j].Timestamp)
		return a.After(b)
	})
	if len(records) > limit {
		records = records[:limit]
	}
	if records == nil {
		records = []requestUsageRecord{}
	}
	return map[string]any{"records": records, "total_records": len(records)}
}

func traeManagementResult(req pluginapi.ManagementRequest, sub string) (map[string]any, error) {
	req.Path = "/v0/management/plugins/trae" + sub
	raw, err := unifiedTraeEngine().Call(pluginabi.MethodManagementHandle, mustJSON(req))
	if err != nil {
		return nil, err
	}
	payload, err := hostBridgeUnwrap(raw, pluginabi.MethodManagementHandle)
	if err != nil {
		return nil, err
	}
	var response pluginapi.ManagementResponse
	if err = json.Unmarshal(payload, &response); err != nil {
		return nil, err
	}
	var out map[string]any
	if err = json.Unmarshal(response.Body, &out); err != nil {
		return nil, err
	}
	if response.StatusCode >= 400 {
		return nil, fmt.Errorf("Trae operation failed: %v", out["error"])
	}
	return out, nil
}

func traePhysical(index string) (*pluginapi.HostAuthGetResponse, error) {
	if strings.TrimSpace(index) == "" {
		return nil, fmt.Errorf("Trae account is required")
	}
	raw, err := traeHostCall(pluginabi.MethodHostAuthGet, mustJSON(pluginapi.HostAuthGetRequest{AuthIndex: index}))
	if err != nil {
		return nil, err
	}
	payload, err := hostBridgeUnwrap(raw, pluginabi.MethodHostAuthGet)
	if err != nil {
		return nil, err
	}
	var phys pluginapi.HostAuthGetResponse
	if err = json.Unmarshal(payload, &phys); err != nil {
		return nil, err
	}
	return &phys, nil
}

func traeModelsForPanel(req pluginapi.ManagementRequest) (map[string]any, error) {
	index := req.Query.Get("auth_index")
	phys, err := traePhysical(index)
	if err != nil {
		return nil, err
	}
	// Return the complete catalog to the editor, while model.for_auth applies
	// visibility settings to the client-facing list.
	raw, err := unifiedTraeEngine().Call(pluginabi.MethodModelForAuth, mustJSON(pluginapi.AuthModelRequest{AuthID: index, StorageJSON: phys.JSON}))
	if err != nil {
		return nil, err
	}
	payload, err := hostBridgeUnwrap(raw, pluginabi.MethodModelForAuth)
	if err != nil {
		return nil, err
	}
	var models pluginapi.ModelResponse
	if err = json.Unmarshal(payload, &models); err != nil {
		return nil, err
	}
	var settings struct {
		Disabled []string `json:"disabled_models"`
	}
	_ = json.Unmarshal(phys.JSON, &settings)
	for i := range models.Models {
		models.Models[i].ID = traeModelPrefix + models.Models[i].ID
	}
	return map[string]any{"models": models.Models, "disabled_models": settings.Disabled}, nil
}

func saveTraeModelSettings(req pluginapi.ManagementRequest) (map[string]any, error) {
	var body struct {
		AuthIndex string   `json:"auth_index"`
		Disabled  []string `json:"disabled_models"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return nil, err
	}
	if len(body.Disabled) > 1000 {
		return nil, fmt.Errorf("too many disabled models")
	}
	phys, err := traePhysical(body.AuthIndex)
	if err != nil {
		return nil, err
	}
	for i, id := range body.Disabled {
		body.Disabled[i] = strings.TrimPrefix(strings.TrimSpace(id), traeModelPrefix)
	}
	_ = phys // ownership was checked before entering the channel's account lock.
	err = unifiedTraeEngine().SetDisabledModels(body.AuthIndex, body.Disabled)
	if err != nil {
		return nil, err
	}
	return map[string]any{"saved": true, "reload": mmTriggerHostReload(req.Headers)}, nil
}
