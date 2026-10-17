package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestHostHTTPResponseSchemas(t *testing.T) {
	for _, shape := range []string{"sdk", "snake"} {
		t.Run(shape, func(t *testing.T) {
			host := func(method string, _ []byte) ([]byte, error) {
				switch method {
				case pluginabi.MethodHostHTTPOperationOpen:
					return OK(map[string]string{"operation_id": "op"}), nil
				case pluginabi.MethodHostHTTPDo:
					if shape == "sdk" {
						return OK(pluginapi.HTTPResponse{StatusCode: 201, Headers: http.Header{"X-Fixture": {"kept"}}, Body: []byte(`{"value":"body"}`)}), nil
					}
					return OK(map[string]any{"status_code": 201, "headers": http.Header{"X-Fixture": {"kept"}}, "body": []byte(`{"value":"body"}`)}), nil
				default:
					return nil, fmt.Errorf("unexpected callback %s", method)
				}
			}
			e := New(host, "test")
			defer e.Stop()
			resp, err := e.hostClient("", "").HTTP.Get("https://fixture.test/json")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != 201 || resp.Header.Get("X-Fixture") != "kept" || string(body) != `{"value":"body"}` {
				t.Fatalf("response lost fields: status=%d headers=%v body=%q err=%v", resp.StatusCode, resp.Header, body, err)
			}
		})
	}
}

func TestRefreshThroughActualHostResponseSchema(t *testing.T) {
	stored := credential("schema-user", "old", time.Now().Add(-time.Hour).Unix())
	exchanges, saves := 0, 0
	host := func(method string, raw []byte) ([]byte, error) {
		switch method {
		case pluginabi.MethodHostAuthGet:
			return OK(pluginapi.HostAuthGetResponse{Name: "trae-schema-user.json", JSON: stored}), nil
		case pluginabi.MethodHostHTTPOperationOpen:
			return OK(map[string]string{"operation_id": "exchange"}), nil
		case pluginabi.MethodHostHTTPDo:
			var req struct {
				Request struct {
					URL  string `json:"url"`
					Body []byte `json:"body"`
				} `json:"request"`
			}
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, err
			}
			if !strings.HasSuffix(req.Request.URL, "/cloudide/api/v3/trae/oauth/ExchangeToken") {
				return nil, fmt.Errorf("wrong exchange endpoint")
			}
			exchanges++
			return OK(pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"Result":{"Token":"fresh","RefreshToken":"rotated","TokenExpireDuration":7200}}`)}), nil
		case pluginabi.MethodHostAuthSave:
			var req pluginapi.HostAuthSaveRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return nil, err
			}
			stored = req.JSON
			saves++
			return OK(nil), nil
		default:
			return nil, fmt.Errorf("unexpected callback %s", method)
		}
	}
	e := New(host, "test")
	defer e.Stop()
	a, _, err := e.ensureAuth("index", stored, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.JWT() != "fresh" || a.RefreshTokenValue() != "rotated" || exchanges != 1 || saves != 1 {
		t.Fatal("refresh did not persist rotated tokens")
	}
}

func TestRedactionKeepsExchangeEndpoint(t *testing.T) {
	err := `Post "https://api.trae.com.cn/cloudide/api/v3/trae/oauth/ExchangeToken": host returned invalid HTTP status (0)`
	if got := redact(err); got != err {
		t.Fatalf("endpoint redacted: %s", got)
	}
	for _, secret := range []string{`{"accessToken":"access-secret","refreshToken":"refresh-secret","Token":"token-secret"}`, `refreshToken=query-secret&foo=bar`, `Authorization: Cloud-IDE-JWT header-secret`} {
		got := redact(secret)
		if strings.Contains(got, "-secret") {
			t.Fatalf("secret exposed: %s", got)
		}
	}
}
