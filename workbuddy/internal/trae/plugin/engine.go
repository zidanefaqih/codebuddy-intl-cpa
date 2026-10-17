package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/zidanefaqih/codebuddy-intl-cpa/workbuddy/internal/trae/upstream"
	"gopkg.in/yaml.v3"
)

const Provider = "trae"

type HostCall func(string, []byte) ([]byte, error)
type Config struct {
	CheckinAuto    bool   `yaml:"checkin_auto" json:"checkin_auto"`
	TokenKeepalive bool   `yaml:"token_keepalive" json:"token_keepalive"`
	CheckinTime    string `yaml:"checkin_time" json:"checkin_time"`
	MaxAttempts    int    `yaml:"max_attempts" json:"max_attempts"`
	UsageFile      string `yaml:"usage_file" json:"usage_file"`
	CallbackPort   int    `yaml:"callback_port" json:"callback_port"`
}
type Engine struct {
	host          HostCall
	version       string
	mu            sync.Mutex
	config        Config
	basePath      string
	cooldown      map[string]time.Time
	accountLocks  sync.Map
	modelCache    map[string]modelCache
	logins        map[string]*pendingLogin
	usage         []Usage
	usageMu       sync.Mutex
	journalMu     sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	startOnce     sync.Once
	callback      *http.Server
	callbackMu    sync.Mutex
	clientFactory func(string, string) *upstream.Client
	usageSink     func(Usage)
}

func New(host HostCall, version string) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{host: host, version: version, config: defaults(), basePath: "/v0/management", cooldown: map[string]time.Time{}, modelCache: map[string]modelCache{}, logins: map[string]*pendingLogin{}, ctx: ctx, cancel: cancel}
	e.clientFactory = e.hostClient
	return e
}
func defaults() Config {
	return Config{CheckinAuto: true, TokenKeepalive: true, CheckinTime: "09:00", MaxAttempts: 3, CallbackPort: 18080}
}
func (e *Engine) settings() Config { e.mu.Lock(); defer e.mu.Unlock(); return e.config }
func (e *Engine) Stop() {
	e.cancel()
	e.callbackMu.Lock()
	if e.callback != nil {
		_ = e.callback.Close()
	}
	e.callbackMu.Unlock()
}
func JSON(v any) []byte { b, _ := json.Marshal(v); return b }
func OK(v any) []byte   { return JSON(map[string]any{"ok": true, "result": v}) }

type WireError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (w *WireError) Error() string   { return w.Message }
func (w *WireError) StatusCode() int { return w.HTTPStatus }

// SetUsageSink connects the channel's existing accounting to the unified journal.
func (e *Engine) SetUsageSink(sink func(Usage)) { e.mu.Lock(); e.usageSink = sink; e.mu.Unlock() }
func ErrorJSON(err error) []byte {
	status := 502
	code := "upstream_error"
	if w, ok := err.(*WireError); ok {
		status = w.HTTPStatus
		code = w.Code
	}
	return JSON(map[string]any{"ok": false, "error": WireError{code, redact(err.Error()), status}})
}

var secretPattern = regexp.MustCompile(`(?i)(?:Cloud-IDE-JWT|Bearer)\s+[^\s"<>]+|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+|\b(?:accessToken|refreshToken|token)\b["\s:=]+[^\s",&<>]+`)

func redact(s string) string {
	s = secretPattern.ReplaceAllString(s, "[redacted]")
	if len(s) > 600 {
		s = s[:600]
	}
	return s
}
func decode(raw []byte, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return &WireError{"invalid_request", "invalid JSON request", 400}
	}
	return nil
}
func (e *Engine) rpc(method string, v any, out any) error {
	if e.host == nil {
		return fmt.Errorf("CPA host callback unavailable")
	}
	raw, err := e.host(method, JSON(v))
	if err != nil {
		return fmt.Errorf("%s: %s", method, redact(err.Error()))
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *WireError      `json:"error"`
	}
	if err = json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s: invalid host envelope", method)
	}
	if !env.OK {
		if env.Error != nil {
			return fmt.Errorf("%s: %s", method, redact(env.Error.Message))
		}
		return fmt.Errorf("%s: host rejected callback", method)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}
func (e *Engine) Call(method string, raw []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		cfg := defaults()
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			return nil, &WireError{"invalid_config", "invalid Trae plugin configuration", 400}
		}
		if cfg.MaxAttempts < 1 || cfg.MaxAttempts > 20 || cfg.CallbackPort < 0 || cfg.CallbackPort > 65535 {
			return nil, &WireError{"invalid_config", "max_attempts must be 1..20; callback_port must be 0..65535", 400}
		}
		if _, err := time.Parse("15:04", cfg.CheckinTime); err != nil {
			return nil, &WireError{"invalid_config", "checkin_time must use HH:MM", 400}
		}
		e.mu.Lock()
		e.config = cfg
		e.mu.Unlock()
		e.startOnce.Do(func() { go e.maintenance() })
		return OK(e.registration()), nil
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier:
		return OK(map[string]string{"identifier": Provider}), nil
	case pluginabi.MethodAuthParse:
		return e.parseAuth(raw)
	case pluginabi.MethodAuthRefresh:
		return e.refreshAuth(raw)
	case pluginabi.MethodAuthLoginStart:
		return e.loginStart(raw)
	case pluginabi.MethodAuthLoginPoll:
		return e.loginPoll(raw)
	case pluginabi.MethodModelStatic:
		return OK(pluginapi.ModelResponse{Provider: Provider, Models: []pluginapi.ModelInfo{}}), nil
	case pluginabi.MethodModelForAuth:
		return e.models(raw)
	case pluginabi.MethodExecutorExecute:
		return e.execute(raw, false)
	case pluginabi.MethodExecutorExecuteStream:
		return e.execute(raw, true)
	case pluginabi.MethodExecutorCountTokens:
		return nil, &WireError{"unsupported", "Trae does not provide a reliable token counting endpoint", 501}
	case pluginabi.MethodManagementRegister:
		var req pluginapi.ManagementRegistrationRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		if req.BasePath != "" {
			e.mu.Lock()
			e.basePath = strings.TrimRight(req.BasePath, "/")
			e.mu.Unlock()
		}
		return OK(e.managementRegistration()), nil
	case pluginabi.MethodManagementHandle:
		return e.manage(raw)
	case pluginabi.MethodSchedulerPick:
		return e.pick(raw)
	default:
		return nil, &WireError{"unknown_method", "unknown plugin method: " + method, 400}
	}
}
func (e *Engine) registration() any {
	return map[string]any{"schema_version": pluginabi.SchemaVersion, "metadata": pluginapi.Metadata{Name: "Trae CPA 国内版", Version: e.version, Author: "Trae CPA contributors", GitHubRepository: "local://trae-cpa", ConfigFields: []pluginapi.ConfigField{
		{Name: "checkin_auto", Type: pluginapi.ConfigFieldTypeBoolean, Description: "每天自动签到"},
		{Name: "checkin_time", Type: pluginapi.ConfigFieldTypeString, Description: "北京时间签到时间，默认 09:00"},
		{Name: "token_keepalive", Type: pluginapi.ConfigFieldTypeBoolean, Description: "每小时检查并刷新即将过期的凭证"},
		{Name: "max_attempts", Type: pluginapi.ConfigFieldTypeInteger, Description: "首个输出前最多尝试的账号数，默认 3"},
		{Name: "callback_port", Type: pluginapi.ConfigFieldTypeInteger, Description: "本机网页登录回调端口，默认 18080；0 使用粘贴回调"},
		{Name: "usage_file", Type: pluginapi.ConfigFieldTypeString, Description: "可选 JSONL 用量日志路径，默认只保留内存记录"},
	}}, "capabilities": map[string]any{"model_provider": true, "auth_provider": true, "executor": true, "executor_model_scope": "oauth", "executor_input_formats": []string{"openai"}, "executor_output_formats": []string{"openai"}, "management_api": true, "scheduler": true}}
}
