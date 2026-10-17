package plugin

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/zidanefaqih/codebuddy-intl-cpa/workbuddy/internal/trae/auth"
	"github.com/zidanefaqih/codebuddy-intl-cpa/workbuddy/internal/trae/upstream"
)

var uidPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func parseCredential(raw []byte) (*auth.Auth, error) {
	if proxyURL(raw) != "" {
		return nil, &WireError{"unsupported_proxy", "CPA 8.0.23 native HTTP callbacks use the global proxy; remove credential proxy_url and configure CPA network.proxy-url", 400}
	}
	a, err := auth.Parse(raw)
	if err != nil {
		return nil, &WireError{"invalid_auth", "credential requires accessToken", 400}
	}
	if a.Domain != "" && a.Domain != "trae.cn" && a.Domain != "trae.com.cn" {
		return nil, &WireError{"invalid_auth", "only Trae CN credentials are supported", 400}
	}
	if a.ApiHost != "" && a.ApiHost != upstream.OAuthHost && a.ApiHost != upstream.UgHost {
		return nil, &WireError{"invalid_auth", "credential apiHost must be a Trae CN OAuth host", 400}
	}
	if a.UID == "" {
		parts := strings.Split(a.JWT(), ".")
		if len(parts) > 1 {
			if b, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
				var claims map[string]any
				if json.Unmarshal(b, &claims) == nil {
					for _, k := range []string{"user_id", "uid", "sub"} {
						if s, ok := claims[k].(string); ok && s != "" {
							a.UID = s
							break
						}
					}
				}
			}
		}
	}
	if !uidPattern.MatchString(a.UID) {
		return nil, &WireError{"invalid_auth", "credential requires a valid account uid", 400}
	}
	a.Domain = "trae.cn"
	if a.ExpiresAt > 1e12 {
		a.ExpiresAt /= 1000
	}
	return a, nil
}
func credentialJSON(a *auth.Auth, original []byte) []byte {
	var obj map[string]any
	decoder := json.NewDecoder(bytes.NewReader(original))
	decoder.UseNumber()
	_ = decoder.Decode(&obj)
	if obj == nil {
		obj = map[string]any{}
	}
	a.RLock()
	defer a.RUnlock()
	nested, _ := obj["auth"].(map[string]any)
	if nested == nil {
		nested = map[string]any{}
	}
	for k, v := range map[string]any{"accessToken": a.AccessToken, "refreshToken": a.RefreshToken, "expiresAt": a.ExpiresAt, "domain": a.Domain, "apiHost": a.ApiHost, "machineId": a.MachineID, "deviceId": a.DeviceID} {
		nested[k] = v
		delete(obj, k)
	}
	account, _ := obj["account"].(map[string]any)
	if account == nil {
		account = map[string]any{}
	}
	account["uid"] = a.UID
	account["nickname"] = a.Nickname
	account["enterpriseId"] = a.EnterpriseID
	obj["type"] = Provider
	obj["auth"] = nested
	obj["account"] = account
	obj["expired"] = time.Unix(a.ExpiresAt, 0).UTC().Format(time.RFC3339)
	return JSON(obj)
}
func authData(a *auth.Auth, raw []byte, id, name string) pluginapi.AuthData {
	var obj map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&obj)
	if id == "" {
		id = "trae-" + a.UID + ".json"
	}
	if name == "" {
		name = "trae-" + a.UID + ".json"
	}
	label, _ := obj["label"].(string)
	if label == "" {
		label = a.Nickname
	}
	if label == "" {
		label = a.UID
	}
	prefix, _ := obj["prefix"].(string)
	proxy, _ := obj["proxy_url"].(string)
	disabled, _ := obj["disabled"].(bool)
	next := time.Now().Add(time.Hour)
	if a.ExpiresAt > 0 {
		next = time.Unix(a.ExpiresAt, 0).Add(-time.Hour)
		if next.Before(time.Now()) {
			next = time.Now().Add(time.Minute)
		}
	}
	return pluginapi.AuthData{Provider: Provider, ID: id, FileName: name, Label: label, Prefix: prefix, ProxyURL: proxy, Disabled: disabled, StorageJSON: raw, Metadata: obj, Attributes: map[string]string{"uid": a.UID}, NextRefreshAfter: next}
}
func (e *Engine) parseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	var probe map[string]any
	_ = json.Unmarshal(req.RawJSON, &probe)
	typ, _ := probe["type"].(string)
	name := req.FileName
	if name == "" {
		name = filepath.Base(req.Path)
	}
	if req.Provider != Provider && typ != Provider && !strings.HasPrefix(strings.ToLower(name), "trae-") {
		return OK(pluginapi.AuthParseResponse{Handled: false}), nil
	}
	if req.Provider != "" && req.Provider != Provider {
		return OK(pluginapi.AuthParseResponse{Handled: false}), nil
	}
	a, err := parseCredential(req.RawJSON)
	if err != nil {
		return nil, err
	}
	storage := credentialJSON(a, req.RawJSON)
	return OK(pluginapi.AuthParseResponse{Handled: true, Auth: authData(a, storage, name, name)}), nil
}
func (e *Engine) listAuths() ([]pluginapi.HostAuthFileEntry, error) {
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := e.rpc(pluginabi.MethodHostAuthList, nil, &resp); err != nil {
		return nil, err
	}
	out := []pluginapi.HostAuthFileEntry{}
	for _, f := range resp.Files {
		if f.Provider == Provider || f.Type == Provider || strings.HasPrefix(strings.ToLower(f.Name), "trae-") {
			out = append(out, f)
		}
	}
	return out, nil
}
func (e *Engine) getAuth(index string) (pluginapi.HostAuthGetResponse, error) {
	var resp pluginapi.HostAuthGetResponse
	err := e.rpc(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: index}, &resp)
	return resp, err
}
func (e *Engine) saveAuth(name string, raw []byte) error {
	if filepath.Base(name) != name || !strings.HasSuffix(name, ".json") {
		return fmt.Errorf("invalid credential filename")
	}
	return e.rpc(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{Name: name, JSON: raw}, nil)
}
func (e *Engine) accountLock(uid string) *sync.Mutex {
	v, _ := e.accountLocks.LoadOrStore(uid, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// Settings and token refresh share the same lock and re-read physical storage.
func (e *Engine) SetDisabledModels(index string, models []string) error {
	physical, err := e.getAuth(index)
	if err != nil {
		return err
	}
	a, err := parseCredential(physical.JSON)
	if err != nil {
		return err
	}
	lock := e.accountLock(a.UID)
	lock.Lock()
	defer lock.Unlock()
	physical, err = e.getAuth(index)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err = json.Unmarshal(physical.JSON, &doc); err != nil {
		return err
	}
	doc["disabled_models"] = JSON(models)
	return e.saveAuth(physical.Name, JSON(doc))
}
func disabled(raw []byte) bool {
	var obj struct{ Disabled bool }
	_ = json.Unmarshal(raw, &obj)
	return obj.Disabled
}
func unavailable(f pluginapi.HostAuthFileEntry) bool {
	return f.Disabled || f.Unavailable || strings.EqualFold(f.Status, "disabled") || strings.EqualFold(f.Status, "error")
}

// Serialize token rotation, then re-read host storage so concurrent requests cannot
// exchange the same rotated refresh token. Persist before making any chat request.
func (e *Engine) ensureAuth(id string, raw []byte, force bool, clientID, callbackID string) (*auth.Auth, []byte, error) {
	a, err := parseCredential(raw)
	if err != nil {
		return nil, nil, err
	}
	lock := e.accountLock(a.UID)
	lock.Lock()
	defer lock.Unlock()
	name := "trae-" + a.UID + ".json"
	previous := a.JWT()
	if id != "" {
		if physical, err := e.getAuth(id); err == nil {
			current, err := parseCredential(physical.JSON)
			if err != nil {
				return nil, nil, err
			}
			if current.UID != a.UID {
				return nil, nil, fmt.Errorf("host credential identity changed")
			}
			a = current
			raw = physical.JSON
			if physical.Name != "" {
				name = physical.Name
			}
			if a.JWT() != previous {
				force = false
			}
		}
	}
	if disabled(raw) {
		return nil, nil, &WireError{"account_disabled", "selected Trae account is disabled", 503}
	}
	changed, err := a.EnsureCheckinDeviceID()
	if err != nil {
		return nil, nil, err
	}
	if a.MachineID == "" {
		a.MachineID, err = randomHex(32)
		if err != nil {
			return nil, nil, err
		}
		changed = true
	}
	if (force || a.NeedsRefresh(time.Hour)) && a.RefreshTokenValue() != "" {
		if err = e.clientFactory(clientID, callbackID).RefreshToken(a); err != nil {
			return nil, nil, err
		}
		changed = true
	}
	if a.ExpiresAt > 0 && a.ExpiresAt <= time.Now().Unix() {
		return nil, nil, &WireError{"expired_auth", "Trae token expired; import a refreshed credential or log in again", 401}
	}
	if changed {
		raw = credentialJSON(a, raw)
		if err = e.saveAuth(name, raw); err != nil {
			return nil, nil, fmt.Errorf("persist refreshed credential: %w", err)
		}
	}
	return a, raw, nil
}
func (e *Engine) refreshAuth(raw []byte) ([]byte, error) {
	var req struct {
		pluginapi.AuthRefreshRequest
		HostCallbackID string `json:"host_callback_id"`
	}
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	if req.AuthProvider != "" && req.AuthProvider != Provider {
		return nil, &WireError{"invalid_auth", "wrong credential provider", 400}
	}
	a, storage, err := e.ensureAuth(req.AuthID, req.StorageJSON, false, "", req.HostCallbackID)
	if err != nil {
		return nil, err
	}
	data := authData(a, storage, req.AuthID, "")
	for k, v := range req.Attributes {
		data.Attributes[k] = v
	}
	data.Metadata = mergeMaps(req.Metadata, data.Metadata)
	return OK(pluginapi.AuthRefreshResponse{Auth: data, NextRefreshAfter: data.NextRefreshAfter}), nil
}
func mergeMaps(base, extra map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	for k, v := range extra {
		base[k] = v
	}
	return base
}

type modelCache struct {
	at     time.Time
	models []pluginapi.ModelInfo
}

func (e *Engine) models(raw []byte) ([]byte, error) {
	var req struct {
		pluginapi.AuthModelRequest
		HostCallbackID string `json:"host_callback_id"`
	}
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	a, storage, err := e.ensureAuth(req.AuthID, req.StorageJSON, false, proxyURL(req.StorageJSON), req.HostCallbackID)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	cached := e.modelCache[a.UID]
	e.mu.Unlock()
	if time.Since(cached.at) < 5*time.Minute {
		return OK(pluginapi.ModelResponse{Provider: Provider, Models: cached.models}), nil
	}
	infos, err := e.clientFactory(proxyURL(storage), req.HostCallbackID).FetchModels(a)
	if err != nil {
		return nil, err
	}
	models := make([]pluginapi.ModelInfo, 0, len(infos))
	for _, m := range infos {
		if strings.EqualFold(m.ID, "work") || strings.HasSuffix(strings.ToLower(m.ID), "-official") {
			continue
		}
		models = append(models, pluginapi.ModelInfo{ID: m.ID, Name: m.ID, DisplayName: m.Name, Object: "model", OwnedBy: Provider, Created: time.Now().Unix(), ContextLength: m.ContextWindow, MaxCompletionTokens: m.MaxTokens, SupportedParameters: []string{"tools", "tool_choice", "parallel_tool_calls"}})
	}
	e.mu.Lock()
	e.modelCache[a.UID] = modelCache{time.Now(), models}
	e.mu.Unlock()
	return OK(pluginapi.ModelResponse{Provider: Provider, Models: models}), nil
}
