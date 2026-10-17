package main

// The host registers one provider per native library. The outer provider stays
// workbuddy; Trae retains its protocol and account pool behind this boundary.
import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	trae "github.com/zidanefaqih/codebuddy-intl-cpa/workbuddy/internal/trae/plugin"
	"gopkg.in/yaml.v3"
)

const traeModelPrefix = "trae/"

var unifiedHostCall = hostCall
var traeUsageStore = &requestUsageStore{logPrefix: "trae-request-usage", perDay: map[string]map[string]*dayAgg{}}
var traeRuntime struct {
	sync.Mutex
	engine *trae.Engine
}

func unifiedTraeEngine() *trae.Engine {
	traeRuntime.Lock()
	defer traeRuntime.Unlock()
	if traeRuntime.engine == nil {
		traeRuntime.engine = trae.New(traeHostCall, version)
		traeRuntime.engine.SetUsageSink(recordTraeUsage)
	}
	return traeRuntime.engine
}

func isTraeCredential(raw []byte) bool {
	var doc struct {
		Type    string `json:"type"`
		Channel string `json:"channel"`
		Auth    struct {
			Domain string `json:"domain"`
		} `json:"auth"`
		Domain string `json:"domain"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	return doc.Channel == "trae" || doc.Type == "trae" ||
		(strings.EqualFold(doc.Auth.Domain, "trae.cn") || strings.EqualFold(doc.Domain, "trae.cn") || strings.EqualFold(doc.Auth.Domain, "trae.com.cn") || strings.EqualFold(doc.Domain, "trae.com.cn"))
}

func managedTraeJSON(raw []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return nil, fmt.Errorf("invalid Trae credential JSON")
	}
	doc["type"] = json.RawMessage(`"workbuddy"`)
	doc["channel"] = json.RawMessage(`"trae"`)
	if _, ok := doc["provider"]; ok {
		doc["provider"] = json.RawMessage(`"workbuddy"`)
	}
	return json.Marshal(doc)
}

func configureTrae(raw []byte) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
		return err
	}
	child := map[string]any{}
	for key, val := range cfg {
		if strings.HasPrefix(key, "trae_") {
			child[strings.TrimPrefix(key, "trae_")] = val
		}
	}
	encoded, err := yaml.Marshal(child)
	if err != nil {
		return err
	}
	request, _ := json.Marshal(map[string]any{"config_yaml": encoded})
	_, err = unifiedTraeEngine().Call(pluginabi.MethodPluginReconfigure, request)
	return err
}

func traeHostCall(method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodHostAuthList:
		result, err := unifiedHostCall(method, raw)
		if err != nil {
			return nil, err
		}
		payload, err := hostBridgeUnwrap(result, method)
		if err != nil {
			return nil, err
		}
		var list rpcHostAuthListResponse
		if err := json.Unmarshal(payload, &list); err != nil {
			return nil, err
		}
		files := make([]pluginapi.HostAuthFileEntry, 0)
		for _, f := range list.Files {
			// Filename is deliberately separate from workbuddy-* so the existing
			// CodeBuddy background tasks cannot act on a Trae credential.
			if !strings.HasPrefix(f.Name, "trae-") || (f.Provider != providerName && f.Type != providerName) {
				continue
			}
			physRaw, err := unifiedHostCall(pluginabi.MethodHostAuthGet, mustJSON(pluginapi.HostAuthGetRequest{AuthIndex: f.AuthIndex}))
			if err != nil {
				continue
			}
			physical, err := hostBridgeUnwrap(physRaw, pluginabi.MethodHostAuthGet)
			if err != nil {
				continue
			}
			var phys pluginapi.HostAuthGetResponse
			if json.Unmarshal(physical, &phys) != nil || !isTraeCredential(phys.JSON) {
				continue
			}
			f.Provider, f.Type = "trae", "trae"
			files = append(files, f)
		}
		return okEnvelope(rpcHostAuthListResponse{Files: files})
	case pluginabi.MethodHostAuthGet:
		result, err := unifiedHostCall(method, raw)
		if err != nil {
			return nil, err
		}
		payload, err := hostBridgeUnwrap(result, method)
		if err != nil {
			return nil, err
		}
		var phys pluginapi.HostAuthGetResponse
		if json.Unmarshal(payload, &phys) != nil || !isTraeCredential(phys.JSON) {
			return nil, fmt.Errorf("credential does not belong to the Trae channel")
		}
		return result, nil
	case pluginabi.MethodHostAuthSave:
		var req pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(req.Name, "trae-") || path.Base(req.Name) != req.Name || strings.Contains(req.Name, `\`) {
			return nil, fmt.Errorf("invalid Trae credential target")
		}
		storage, err := managedTraeJSON(req.JSON)
		if err != nil {
			return nil, err
		}
		req.JSON = storage
		result, err := unifiedHostCall(method, mustJSON(req))
		if err == nil {
			invalidateAuthListCache()
		}
		return result, err
	default:
		return unifiedHostCall(method, raw)
	}
}

func translateTraeAuth(ad *pluginapi.AuthData, parsing, refreshing bool) error {
	if len(ad.StorageJSON) == 0 {
		return nil
	}
	raw, err := managedTraeJSON(ad.StorageJSON)
	if err != nil {
		return err
	}
	ad.StorageJSON, ad.Provider = raw, providerName
	if ad.Metadata == nil {
		ad.Metadata = map[string]any{}
	}
	ad.Metadata["type"], ad.Metadata["channel"] = providerName, "trae"
	if _, ok := ad.Metadata["provider"]; ok {
		ad.Metadata["provider"] = providerName
	}
	if ad.Attributes == nil {
		ad.Attributes = map[string]string{}
	}
	ad.Attributes["channel"] = "trae"
	// Let the host keep the file-derived record identity across watcher reloads.
	if parsing || refreshing {
		ad.ID = ""
	}
	if refreshing {
		ad.FileName = ""
	}
	return nil
}

func dispatchTrae(method string, raw []byte) ([]byte, bool, error) {
	var probe struct {
		RawJSON, StorageJSON []byte
		Model                string
		AuthAttributes       map[string]string
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, false, nil
	}
	isTrae := isTraeCredential(probe.StorageJSON) || isTraeCredential(probe.RawJSON)
	if method == pluginabi.MethodExecutorExecute || method == pluginabi.MethodExecutorExecuteStream {
		resolved := probe.Model
		modelAliasCache.RLock()
		if mapped := modelAliasCache.byAlias[resolved]; mapped != "" {
			resolved = mapped
		}
		modelAliasCache.RUnlock()
		if strings.HasPrefix(resolved, traeModelPrefix) != isTrae {
			if isTrae || strings.HasPrefix(resolved, traeModelPrefix) {
				return nil, true, fmt.Errorf("model and credential channels do not match")
			}
		}
		probe.Model = resolved
	}
	if !isTrae {
		return nil, false, nil
	}
	switch method {
	case pluginabi.MethodAuthParse, pluginabi.MethodAuthRefresh, pluginabi.MethodModelForAuth,
		pluginabi.MethodExecutorExecute, pluginabi.MethodExecutorExecuteStream:
	default:
		return nil, false, nil
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, true, err
	}
	request["Provider"], request["AuthProvider"] = json.RawMessage(`"trae"`), json.RawMessage(`"trae"`)
	if method == pluginabi.MethodAuthParse {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(probe.RawJSON, &doc); err != nil {
			return nil, true, err
		}
		doc["type"] = json.RawMessage(`"trae"`)
		request["RawJSON"] = mustJSON(mustJSON(doc))
	}
	if method == pluginabi.MethodExecutorExecute || method == pluginabi.MethodExecutorExecuteStream {
		model := strings.TrimPrefix(probe.Model, traeModelPrefix)
		if model == "" {
			return nil, true, fmt.Errorf("Trae model is required")
		}
		request["Model"] = mustJSON(model)
	}
	result, err := unifiedTraeEngine().Call(method, mustJSON(request))
	if err != nil {
		return nil, true, err
	}
	payload, err := hostBridgeUnwrap(result, method)
	if err != nil {
		return nil, true, err
	}
	switch method {
	case pluginabi.MethodAuthParse:
		var resp pluginapi.AuthParseResponse
		if err = json.Unmarshal(payload, &resp); err == nil && resp.Handled {
			err = translateTraeAuth(&resp.Auth, true, false)
		}
		result, _ = okEnvelope(resp)
	case pluginabi.MethodAuthRefresh:
		var resp pluginapi.AuthRefreshResponse
		if err = json.Unmarshal(payload, &resp); err == nil {
			err = translateTraeAuth(&resp.Auth, false, true)
		}
		result, _ = okEnvelope(resp)
	case pluginabi.MethodModelForAuth:
		var resp pluginapi.ModelResponse
		if err = json.Unmarshal(payload, &resp); err != nil {
			break
		}
		resp.Provider = providerName
		var settings struct {
			Excluded     []string `json:"excluded_models"`
			HostExcluded []string `json:"excluded-models"`
			Disabled     []string `json:"disabled_models"`
		}
		_ = json.Unmarshal(probe.StorageJSON, &settings)
		models := make([]pluginapi.ModelInfo, 0, len(resp.Models))
		for _, m := range resp.Models {
			id := m.ID
			m.ID, m.Name, m.OwnedBy = traeModelPrefix+id, traeModelPrefix+id, providerName
			patterns := append(append(settings.Excluded, settings.HostExcluded...), settings.Disabled...)
			if modelExcluded(id, m.ID, patterns) {
				continue
			}
			models = append(models, m)
		}
		var req pluginapi.AuthModelRequest
		_ = json.Unmarshal(raw, &req)
		cacheModelAliases(req.Host)
		resp.Models = filterExcludedModels(models, req.Host)
		result, _ = okEnvelope(resp)
	}
	return result, true, err
}

func modelExcluded(id, full string, patterns []string) bool {
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		for _, candidate := range []string{strings.ToLower(id), strings.ToLower(full)} {
			if matched, _ := path.Match(p, candidate); matched {
				return true
			}
		}
	}
	return false
}

func recordTraeUsage(u trae.Usage) {
	detail := usageDetailFromMap(u.Tokens)
	credit := firstUsageNumber(u.Tokens, "credits_consumed", "credit", "creditsFloat")
	rec := requestUsageRecord{Timestamp: u.Time, AuthID: u.Account, AuthIndex: "trae:" + u.Account,
		Nickname: u.Account, Region: "trae", Channel: "trae", Model: traeModelPrefix + u.Model,
		InputTokens: detail.InputTokens, OutputTokens: detail.OutputTokens, TotalTokens: detail.TotalTokens,
		CacheReadTokens: detail.CachedTokens, CacheWriteTokens: detail.CacheCreationTokens, ReasoningTokens: detail.ReasoningTokens, Credit: credit, Failed: !u.Success, LatencyMS: u.DurationMS}
	if detail.CacheReadTokens > rec.CacheReadTokens {
		rec.CacheReadTokens = detail.CacheReadTokens
	}
	traeUsageStore.record(rec)
}

func firstUsageNumber(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k].(float64); ok {
			return v
		}
	}
	return 0
}
