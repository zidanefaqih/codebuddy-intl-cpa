package plugin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"encoding/json"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/zidanefaqih/codebuddy-intl-cpa/workbuddy/internal/trae/upstream"
)

type hostTransport struct {
	e                    *Engine
	clientID, callbackID string
}

func proxyURL(raw []byte) string {
	var obj struct {
		ProxyURL string `json:"proxy_url"`
	}
	_ = json.Unmarshal(raw, &obj)
	return obj.ProxyURL
}

func (e *Engine) hostClient(clientID, callbackID string) *upstream.Client {
	c := upstream.New()
	tr := &hostTransport{e: e, clientID: clientID, callbackID: callbackID}
	c.HTTP.Transport = tr
	c.StreamHTTP.Transport = tr
	return c
}
func (t *hostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	var body []byte
	var err error
	if r.Body != nil {
		body, err = io.ReadAll(io.LimitReader(r.Body, 8<<20+1))
		_ = r.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(body) > 8<<20 {
			return nil, fmt.Errorf("request body too large")
		}
	}
	wire := map[string]any{"host_callback_id": t.callbackID, "request": map[string]any{"method": r.Method, "url": r.URL.String(), "headers": r.Header, "body": body}}
	var operation struct {
		ID string `json:"operation_id"`
	}
	if err = t.e.rpc(pluginabi.MethodHostHTTPOperationOpen, map[string]string{"host_callback_id": t.callbackID}, &operation); err != nil {
		return nil, err
	}
	if operation.ID == "" {
		return nil, fmt.Errorf("host returned empty HTTP operation ID")
	}
	wire["operation_id"] = operation.ID
	stop := make(chan struct{})
	go func() {
		select {
		case <-r.Context().Done():
			_ = t.e.rpc(pluginabi.MethodHostHTTPCancel, map[string]string{"operation_id": operation.ID, "host_callback_id": t.callbackID}, nil)
		case <-stop:
		}
	}()
	keep := false
	defer func() {
		if !keep {
			close(stop)
		}
	}()
	var resp struct {
		// host.http.do serializes pluginapi.HTTPResponse (StatusCode/Headers/Body).
		// host.http.do_stream serializes a separate snake_case RPC schema.
		pluginapi.HTTPResponse
		StatusCodeSnake *int   `json:"status_code"`
		StreamID        string `json:"stream_id"`
	}
	method := pluginabi.MethodHostHTTPDo
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		method = pluginabi.MethodHostHTTPDoStream
	}
	if err = t.e.rpc(method, wire, &resp); err != nil {
		return nil, err
	}
	if resp.StatusCodeSnake != nil {
		resp.StatusCode = *resp.StatusCodeSnake
	}
	if resp.StatusCode < 100 || resp.StatusCode > 599 {
		return nil, fmt.Errorf("host returned invalid HTTP status (%d)", resp.StatusCode)
	}
	var reader io.ReadCloser = io.NopCloser(bytes.NewReader(resp.Body))
	if method == pluginabi.MethodHostHTTPDoStream {
		if resp.StreamID == "" {
			return nil, fmt.Errorf("host did not return an upstream stream ID")
		}
		keep = true
		reader = &hostBody{e: t.e, id: resp.StreamID, ctx: r.Context(), stop: stop}
	}
	return &http.Response{StatusCode: resp.StatusCode, Header: resp.Headers, Body: reader, Request: r}, nil
}

type hostBody struct {
	e        *Engine
	id       string
	ctx      context.Context
	pending  []byte
	done     bool
	terminal error
	once     sync.Once
	stop     chan struct{}
}

func (b *hostBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		if b.terminal != nil {
			return 0, b.terminal
		}
		if b.done {
			return 0, io.EOF
		}
		if err := b.ctx.Err(); err != nil {
			_ = b.Close()
			return 0, err
		}
		var result struct {
			Payload []byte `json:"payload"`
			Error   string `json:"error"`
			Done    bool   `json:"done"`
		}
		if err := b.e.rpc(pluginabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": b.id}, &result); err != nil {
			return 0, err
		}
		b.pending = result.Payload
		b.done = result.Done
		if result.Error != "" {
			b.terminal = fmt.Errorf("%s", redact(result.Error))
		}
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}
func (b *hostBody) Close() error {
	var err error
	b.once.Do(func() {
		if b.stop != nil {
			close(b.stop)
		}
		err = b.e.rpc(pluginabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": b.id}, nil)
	})
	return err
}
