package plugin

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/zidanefaqih/codebuddy-intl-cpa/workbuddy/internal/trae/auth"
	"github.com/zidanefaqih/codebuddy-intl-cpa/workbuddy/internal/trae/login"
)

type pendingLogin struct {
	machine, device, trace string
	expires                time.Time
	status                 pluginapi.AuthLoginStatus
	data                   pluginapi.AuthData
	message                string
	busy                   bool
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func (e *Engine) loginStart(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginStartRequest
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	machine, err := randomHex(32)
	if err != nil {
		return nil, err
	}
	device, err := auth.NewCheckinDeviceID()
	if err != nil {
		return nil, err
	}
	state, err := randomHex(24)
	if err != nil {
		return nil, err
	}
	cfg := e.settings()
	port := cfg.CallbackPort
	if port == 0 {
		port = 18080
	}
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/authorize", port)
	auto := false
	notice := "登录完成后，把浏览器地址栏中的完整回调链接粘贴到面板。"
	if cfg.CallbackPort != 0 {
		if err := e.startCallback(cfg.CallbackPort); err == nil {
			auto = true
			notice = "本机浏览器登录后自动接收回调；远程浏览器可粘贴完整回调链接。"
		} else {
			notice = "本地回调端口不可用，请粘贴登录后的完整回调链接。"
		}
	}
	p := &pendingLogin{machine: machine, device: device, trace: device, expires: time.Now().Add(5 * time.Minute), status: pluginapi.AuthLoginStatusPending}
	e.mu.Lock()
	for k, v := range e.logins {
		if time.Now().After(v.expires) {
			delete(e.logins, k)
		}
	}
	if len(e.logins) >= 64 {
		e.mu.Unlock()
		return nil, fmt.Errorf("too many pending login sessions")
	}
	e.logins[state] = p
	e.mu.Unlock()
	return OK(pluginapi.AuthLoginStartResponse{Provider: Provider, State: state, URL: login.BuildLoginURL(machine, device, callbackURL), ExpiresAt: p.expires, Metadata: map[string]any{"callback_url": callbackURL, "automatic_callback": auto, "message": notice}}), nil
}
func (e *Engine) startCallback(port int) error {
	e.callbackMu.Lock()
	defer e.callbackMu.Unlock()
	if e.callback != nil {
		if e.callback.Addr != fmt.Sprintf("127.0.0.1:%d", port) {
			return fmt.Errorf("callback port changed; restart CPA")
		}
		return nil
	}
	address := fmt.Sprintf("127.0.0.1:%d", port)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		if len(r.URL.RawQuery) > 64<<10 {
			w.WriteHeader(413)
			return
		}
		if err := e.completeLogin("", r.URL.String()); err != nil {
			w.WriteHeader(400)
			_, _ = fmt.Fprint(w, "Trae 登录未完成，请返回 CPA 面板重新发起登录。")
			return
		}
		_, _ = fmt.Fprint(w, "Trae 登录成功，账号已导入 CPA。可以关闭本页面。")
	})
	srv := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10}
	e.callback = srv
	go func() { _ = srv.Serve(listener) }()
	return nil
}
func (e *Engine) completeLogin(state, callbackURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(callbackURL))
	if err != nil {
		return fmt.Errorf("invalid callback URL")
	}
	trace := parsed.Query().Get("loginTraceID")
	if trace == "" {
		trace = parsed.Query().Get("login_trace_id")
	}
	e.mu.Lock()
	p := e.logins[state]
	if state == "" {
		for k, v := range e.logins {
			if trace != "" && v.trace == trace {
				state = k
				p = v
				break
			}
		}
	}
	if p == nil || time.Now().After(p.expires) || p.status != pluginapi.AuthLoginStatusPending || p.busy {
		e.mu.Unlock()
		return fmt.Errorf("unknown, expired or completed login session")
	}
	if trace == "" || trace != p.trace {
		e.mu.Unlock()
		return fmt.Errorf("callback does not match this login session")
	}
	p.busy = true
	e.mu.Unlock()
	info, err := login.ParseCallback(callbackURL)
	var data pluginapi.AuthData
	if err == nil {
		a := &auth.Auth{AccessToken: info.AccessToken, RefreshToken: info.RefreshToken, ExpiresAt: info.ExpiresAt, UID: info.UID, Nickname: info.Nickname, EnterpriseID: info.EnterpriseID, MachineID: p.machine, DeviceID: p.device, Domain: "trae.cn"}
		client := e.clientFactory("", "")
		if a.RefreshToken != "" {
			err = client.RefreshToken(a)
		}
		if err == nil {
			uid, nick, enterprise, userErr := client.GetUserInfo(a)
			if userErr == nil {
				a.UID = uid
				a.Nickname = nick
				a.EnterpriseID = enterprise
			} else if a.UID == "" {
				err = userErr
			}
		}
		if err == nil {
			storage := credentialJSON(a, nil)
			_, err = parseCredential(storage)
			if err == nil {
				err = e.saveAuth("trae-"+a.UID+".json", storage)
				data = authData(a, storage, "", "")
			}
		}
	}
	e.mu.Lock()
	p.busy = false
	if err != nil {
		p.status = pluginapi.AuthLoginStatusError
		p.message = redact(err.Error())
	} else {
		p.status = pluginapi.AuthLoginStatusSuccess
		p.data = data
	}
	e.mu.Unlock()
	return err
}
func (e *Engine) loginPoll(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.logins[req.State]
	if p == nil {
		return OK(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "登录会话不存在，请重新登录"}), nil
	}
	if time.Now().After(p.expires) {
		delete(e.logins, req.State)
		return OK(pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "登录已超时，请重新登录"}), nil
	}
	return OK(pluginapi.AuthLoginPollResponse{Status: p.status, Message: p.message, Auth: p.data}), nil
}
