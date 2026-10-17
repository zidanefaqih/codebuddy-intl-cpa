package plugin

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

//go:embed panel.html
var panelHTML []byte

func (e *Engine) managementRegistration() any {
	routes := []map[string]string{}
	for _, route := range [][2]string{{"GET", "/accounts"}, {"POST", "/import"}, {"POST", "/toggle"}, {"POST", "/checkin"}, {"POST", "/refresh"}, {"GET", "/usage"}, {"GET", "/settings"}, {"POST", "/login/start"}, {"POST", "/login/poll"}, {"POST", "/login/complete"}} {
		routes = append(routes, map[string]string{"method": route[0], "path": "/plugins/trae" + route[1]})
	}
	return map[string]any{"routes": routes, "resources": []map[string]string{{"path": "/panel", "menu": "Trae 国内版", "description": "Trae 账号、积分、签到和请求记录"}}}
}
func managementJSON(status int, value any) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}, "Cache-Control": []string{"no-store"}}, Body: JSON(value)}
}
func (e *Engine) manage(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	e.mu.Lock()
	base := e.basePath
	e.mu.Unlock()
	path := strings.TrimPrefix(req.Path, base)
	path = strings.TrimPrefix(path, "/v0/resource/plugins/trae")
	path = strings.TrimPrefix(path, "/plugins/trae")
	if req.Method == "GET" && path == "/panel" {
		return OK(pluginapi.ManagementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}, "Cache-Control": []string{"no-store"}, "X-Content-Type-Options": []string{"nosniff"}, "Content-Security-Policy": []string{"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'self'"}}, Body: panelHTML}), nil
	}
	if len(req.Body) > 8<<20 {
		return OK(managementJSON(413, map[string]string{"error": "请求超过 8 MiB"})), nil
	}
	var result any
	var err error
	switch {
	case req.Method == "GET" && path == "/settings":
		result = e.settings()
	case req.Method == "GET" && path == "/usage":
		result = map[string]any{"records": e.usageSnapshot()}
	case req.Method == "GET" && path == "/accounts":
		result, err = e.accounts()
	case req.Method == "POST" && path == "/import":
		result, err = e.importAuth(req.Body)
	case req.Method == "POST" && path == "/toggle":
		result, err = e.toggle(req.Body)
	case req.Method == "POST" && (path == "/checkin" || path == "/refresh"):
		result, err = e.batch(path, req.Body)
	case req.Method == "POST" && path == "/login/start":
		return e.managementRPC(e.loginStart([]byte(`{}`)))
	case req.Method == "POST" && path == "/login/poll":
		return e.managementRPC(e.loginPoll(req.Body))
	case req.Method == "POST" && path == "/login/complete":
		var r struct {
			State       string `json:"state"`
			CallbackURL string `json:"callback_url"`
		}
		err = decode(req.Body, &r)
		if err == nil {
			err = e.completeLogin(r.State, r.CallbackURL)
		}
		result = map[string]bool{"ok": err == nil}
	default:
		return OK(managementJSON(404, map[string]string{"error": "not found"})), nil
	}
	if err != nil {
		status := 400
		if w, ok := err.(*WireError); ok {
			status = w.HTTPStatus
		}
		return OK(managementJSON(status, map[string]string{"error": redact(err.Error())})), nil
	}
	return OK(managementJSON(200, result)), nil
}
func (e *Engine) managementRPC(raw []byte, err error) ([]byte, error) {
	if err != nil {
		return OK(managementJSON(400, map[string]string{"error": redact(err.Error())})), nil
	}
	var env struct{ Result any }
	_ = json.Unmarshal(raw, &env)
	return OK(managementJSON(200, env.Result)), nil
}
func (e *Engine) importAuth(raw []byte) (any, error) {
	a, err := parseCredential(raw)
	if err != nil {
		return nil, err
	}
	lock := e.accountLock(a.UID)
	lock.Lock()
	defer lock.Unlock()
	name := "trae-" + a.UID + ".json"
	if current, readErr := e.getAuth(name); readErr == nil {
		// Re-importing a token for the same account retains CPA settings absent
		// from the imported document. Explicit fields in the import still win.
		if old, parseErr := parseCredential(current.JSON); parseErr == nil && old.UID == a.UID {
			raw = mergeCredentialFields(current.JSON, raw)
			if a.MachineID == "" {
				a.MachineID = old.MachineID
			}
			if a.DeviceID == "" {
				a.DeviceID = old.DeviceID
			}
			if a.RefreshToken == "" {
				a.RefreshToken = old.RefreshToken
			}
			if a.Nickname == "" {
				a.Nickname = old.Nickname
			}
			if a.EnterpriseID == "" {
				a.EnterpriseID = old.EnterpriseID
			}
		}
	}
	if _, err = a.EnsureCheckinDeviceID(); err != nil {
		return nil, err
	}
	if a.MachineID == "" {
		a.MachineID, err = randomHex(32)
		if err != nil {
			return nil, err
		}
	}
	storage := credentialJSON(a, raw)
	if err = e.saveAuth(name, storage); err != nil {
		return nil, err
	}
	return map[string]string{"uid": a.UID, "nickname": a.Nickname}, nil
}

func mergeCredentialFields(base, extra []byte) []byte {
	var previous, incoming map[string]json.RawMessage
	_ = json.Unmarshal(base, &previous)
	_ = json.Unmarshal(extra, &incoming)
	if previous == nil {
		previous = map[string]json.RawMessage{}
	}
	for k, v := range incoming {
		if k == "auth" || k == "account" {
			var a, b map[string]json.RawMessage
			_ = json.Unmarshal(previous[k], &a)
			_ = json.Unmarshal(v, &b)
			if a != nil && b != nil {
				for field, value := range b {
					a[field] = value
				}
				v = JSON(a)
			}
		}
		previous[k] = v
	}
	return JSON(previous)
}

type AccountView struct {
	Index          string `json:"auth_index"`
	UID            string `json:"uid"`
	Name           string `json:"name"`
	Disabled       bool   `json:"disabled"`
	Remain         int64  `json:"remain"`
	Limit          int64  `json:"limit"`
	Used           int64  `json:"used"`
	Packs          int    `json:"packs"`
	CheckedIn      bool   `json:"checked_in"`
	CheckinEnabled bool   `json:"checkin_enabled"`
	ExpiresAt      int64  `json:"expires_at"`
	Error          string `json:"error,omitempty"`
}

func (e *Engine) accounts() (any, error) {
	files, err := e.listAuths()
	if err != nil {
		return nil, err
	}
	views := []AccountView{}
	for _, f := range files {
		view := AccountView{Index: f.AuthIndex, Name: f.Label, Disabled: f.Disabled}
		physical, err := e.getAuth(f.AuthIndex)
		if err != nil {
			view.Error = redact(err.Error())
			views = append(views, view)
			continue
		}
		a, err := parseCredential(physical.JSON)
		if err != nil {
			view.Error = redact(err.Error())
			views = append(views, view)
			continue
		}
		view.UID = a.UID
		view.ExpiresAt = a.ExpiresAt
		view.Disabled = view.Disabled || disabled(physical.JSON)
		if view.Name == "" {
			view.Name = a.Nickname
		}
		if view.Name == "" {
			view.Name = a.UID
		}
		client := e.clientFactory("", "")
		view.Remain, view.Limit, view.Used, view.Packs, err = client.EntUsage(a)
		if err != nil {
			view.Error = redact(err.Error())
		}
		view.CheckedIn, _, view.CheckinEnabled, err = client.CheckinStatus(a)
		if err != nil {
			view.Error = redact(err.Error())
		}
		views = append(views, view)
	}
	return map[string]any{"accounts": views}, nil
}
func (e *Engine) toggle(raw []byte) (any, error) {
	var req struct {
		Index   string `json:"auth_index"`
		Enabled *bool  `json:"enabled"`
	}
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	if req.Index == "" || req.Enabled == nil {
		return nil, fmt.Errorf("auth_index 和 enabled 为必填")
	}
	physical, err := e.getAuth(req.Index)
	if err != nil {
		return nil, err
	}
	a, err := parseCredential(physical.JSON)
	if err != nil {
		return nil, err
	}
	lock := e.accountLock(a.UID)
	lock.Lock()
	defer lock.Unlock()
	physical, err = e.getAuth(req.Index)
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err = json.Unmarshal(physical.JSON, &obj); err != nil {
		return nil, err
	}
	obj["disabled"] = JSON(!*req.Enabled)
	if err = e.saveAuth(physical.Name, JSON(obj)); err != nil {
		return nil, err
	}
	return map[string]bool{"enabled": *req.Enabled}, nil
}
func (e *Engine) batch(action string, raw []byte) (any, error) {
	var req struct {
		Index string `json:"auth_index"`
	}
	if len(raw) > 0 {
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
	}
	files, err := e.listAuths()
	if err != nil {
		return nil, err
	}
	results := []map[string]any{}
	found := req.Index == ""
	for _, f := range files {
		if req.Index != "" && req.Index != f.AuthIndex {
			continue
		}
		found = true
		if unavailable(f) {
			continue
		}
		row := map[string]any{"auth_index": f.AuthIndex, "success": false}
		physical, err := e.getAuth(f.AuthIndex)
		if err == nil {
			a, _, authErr := e.ensureAuth(f.AuthIndex, physical.JSON, action == "/refresh", "", "")
			err = authErr
			if err == nil && action == "/checkin" {
				client := e.clientFactory("", "")
				checked, _, enabled, statusErr := client.CheckinStatus(a)
				err = statusErr
				if err == nil && enabled && !checked {
					err = client.CheckinClaim(a)
				}
				row["already_checked_in"] = checked
				row["checkin_enabled"] = enabled
			}
		}
		if err != nil {
			row["error"] = redact(err.Error())
		} else {
			row["success"] = true
		}
		results = append(results, row)
	}
	if !found {
		return nil, fmt.Errorf("未找到 Trae 账号")
	}
	return map[string]any{"results": results}, nil
}
