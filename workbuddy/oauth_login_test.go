// oauth_login_test.go covers the realm-aware OAuth login flow (issue #5).
//
// The regression it guards: handleStartLogin posted auth/state to
// copilot.tencent.com/v2/plugin/auth/state?platform=CLI unconditionally. That
// issuer answers with an authUrl on copilot.tencent.com/login, a page that
// redirects a Global account to codebuddy.cn where the flow cannot complete.
// The Intl issuer is www.codebuddy.ai with platform=ide; its authUrl stays on
// .ai, and the polls must follow the issuer that minted the state.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Stub hosts stand in for the two login pages. Nothing resolves them: they only
// have to appear in the authUrl the plugin hands the browser.
const (
	stubHostIntl = "www.codebuddy.ai"
	stubHostCN   = "copilot.tencent.com"
)

// loginIssuerStub fakes one realm's CodeBuddy OAuth issuer — auth/state mints a
// state, auth/token returns the token bundle, login/account returns the profile
// (401 without a bearer, like the real gateway) — and records every request URI
// so a test can assert which realm was actually contacted.
type loginIssuerStub struct {
	*httptest.Server

	host  string // host its authUrl points at
	realm string // realm tag, so payloads are distinguishable

	mu    sync.Mutex
	paths []string
}

func newLoginIssuerStub(t *testing.T, realm, host string) *loginIssuerStub {
	t.Helper()
	stub := &loginIssuerStub{host: host, realm: realm}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.paths = append(stub.paths, r.URL.RequestURI())
		stub.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/state"):
			_, _ = fmt.Fprintf(w, `{"code":0,"msg":"OK","data":{"state":"state-%s","authUrl":"https://%s/login?platform=ide&state=state-%s"}}`, realm, host, realm)
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/token"):
			_, _ = fmt.Fprintf(w, `{"code":0,"msg":"OK","data":{"accessToken":"at-%s","refreshToken":"rt-%s","expiresIn":3600,"domain":"%s"}}`, realm, realm, host)
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/login/account"):
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = fmt.Fprintf(w, `{"code":0,"msg":"OK","data":{"uid":"uid-%s","nickname":"nick-%s"}}`, realm, realm)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(stub.Close)
	return stub
}

// servedPaths returns a copy of the request URIs the stub answered, in order.
func (s *loginIssuerStub) servedPaths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

// useLoginIssuers points the login flow at the stubs and restores the
// production bases and region afterwards.
func useLoginIssuers(t *testing.T, intl, cn *loginIssuerStub, region string) {
	t.Helper()
	origIntl, origCN := loginBaseGlobal, loginBaseCN
	loginBaseGlobal, loginBaseCN = intl.URL, cn.URL
	if _, ok := setOAuthRegionRuntime(region); !ok {
		t.Fatalf("setOAuthRegionRuntime(%q) was rejected", region)
	}
	t.Cleanup(func() {
		loginBaseGlobal, loginBaseCN = origIntl, origCN
		_, _ = setOAuthRegionRuntime(oauthRegionIntl)
	})
}

// envelopeResult unwraps {"ok":true,"result":{...}} into out.
func envelopeResult(t *testing.T, raw []byte, out any) {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v (raw=%s)", err, raw)
	}
	if !env.OK {
		t.Fatalf("envelope reported failure: %s", raw)
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		t.Fatalf("decode result: %v (raw=%s)", err, env.Result)
	}
}

// startLoginThroughStubs drives one start_login call and returns its response.
func startLoginThroughStubs(t *testing.T) pluginapi.AuthLoginStartResponse {
	t.Helper()
	raw, err := handleStartLogin(nil)
	if err != nil {
		t.Fatalf("start login: %v", err)
	}
	var out pluginapi.AuthLoginStartResponse
	envelopeResult(t, raw, &out)
	return out
}

// TestStartLoginIntlIssuerStaysOnAI is the issue-#5 fix itself: with the default
// (Intl) region the login URL must be served by codebuddy.ai — not by the CN
// host whose login page redirects the browser to codebuddy.cn.
func TestStartLoginIntlIssuerStaysOnAI(t *testing.T) {
	intl := newLoginIssuerStub(t, oauthRegionIntl, stubHostIntl)
	cn := newLoginIssuerStub(t, oauthRegionCN, stubHostCN)
	useLoginIssuers(t, intl, cn, oauthRegionIntl)

	resp := startLoginThroughStubs(t)
	defer loginStates.Delete(resp.State)

	if !strings.HasPrefix(resp.URL, "https://"+stubHostIntl+"/login") {
		t.Fatalf("authUrl = %q, want a %s login page", resp.URL, stubHostIntl)
	}
	if got := resp.Metadata["region"]; got != oauthRegionIntl {
		t.Fatalf("metadata region = %v, want %q", got, oauthRegionIntl)
	}
	if paths := intl.servedPaths(); len(paths) != 1 || paths[0] != "/v2/plugin/auth/state?platform=ide" {
		t.Fatalf("Intl issuer paths = %v, want [/v2/plugin/auth/state?platform=ide]", paths)
	}
	if paths := cn.servedPaths(); len(paths) != 0 {
		t.Fatalf("CN issuer was contacted during an Intl login: %v", paths)
	}
}

// TestStartLoginCNIssuerWhenConfigured keeps the CN flow available: with
// oauth_region=cn the state is minted on the CN host (platform=CLI, unchanged)
// and its authUrl is the CN login page.
func TestStartLoginCNIssuerWhenConfigured(t *testing.T) {
	intl := newLoginIssuerStub(t, oauthRegionIntl, stubHostIntl)
	cn := newLoginIssuerStub(t, oauthRegionCN, stubHostCN)
	useLoginIssuers(t, intl, cn, oauthRegionCN)

	resp := startLoginThroughStubs(t)
	defer loginStates.Delete(resp.State)

	if !strings.HasPrefix(resp.URL, "https://"+stubHostCN+"/login") {
		t.Fatalf("authUrl = %q, want a %s login page", resp.URL, stubHostCN)
	}
	if got := resp.Metadata["region"]; got != oauthRegionCN {
		t.Fatalf("metadata region = %v, want %q", got, oauthRegionCN)
	}
	if paths := cn.servedPaths(); len(paths) != 1 || paths[0] != "/v2/plugin/auth/state?platform=CLI" {
		t.Fatalf("CN issuer paths = %v, want [/v2/plugin/auth/state?platform=CLI]", paths)
	}
	if paths := intl.servedPaths(); len(paths) != 0 {
		t.Fatalf("Intl issuer was contacted during a CN login: %v", paths)
	}
}

// TestPollLoginKeepsTheIssuerOfTheState asserts the poll (token + account)
// talks to the gateway that minted the state, for both realms. A state is
// realm-scoped: polling the other issuer is what leaves a login stuck forever.
func TestPollLoginKeepsTheIssuerOfTheState(t *testing.T) {
	cases := []struct {
		name    string
		region  string
		wantURL string
	}{
		{"intl", oauthRegionIntl, "https://" + stubHostIntl + "/login"},
		{"cn", oauthRegionCN, "https://" + stubHostCN + "/login"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			intl := newLoginIssuerStub(t, oauthRegionIntl, stubHostIntl)
			cn := newLoginIssuerStub(t, oauthRegionCN, stubHostCN)
			useLoginIssuers(t, intl, cn, tc.region)

			started := startLoginThroughStubs(t)
			if !strings.HasPrefix(started.URL, tc.wantURL) {
				t.Fatalf("authUrl = %q, want prefix %q", started.URL, tc.wantURL)
			}

			pollBody, err := json.Marshal(pluginapi.AuthLoginPollRequest{Provider: providerName, State: started.State})
			if err != nil {
				t.Fatalf("marshal poll request: %v", err)
			}
			raw, err := handlePollLogin(pollBody)
			if err != nil {
				t.Fatalf("poll login: %v", err)
			}
			var poll pluginapi.AuthLoginPollResponse
			envelopeResult(t, raw, &poll)
			if poll.Status != pluginapi.AuthLoginStatusSuccess {
				t.Fatalf("poll status = %q, want %q (message: %s)", poll.Status, pluginapi.AuthLoginStatusSuccess, poll.Message)
			}

			var stored storedAuth
			if err := json.Unmarshal(poll.Auth.StorageJSON, &stored); err != nil {
				t.Fatalf("decode stored auth: %v (raw=%s)", err, poll.Auth.StorageJSON)
			}
			if stored.Auth.AccessToken != "at-"+tc.region {
				t.Fatalf("accessToken = %q, want at-%s", stored.Auth.AccessToken, tc.region)
			}
			if stored.Auth.RefreshToken != "rt-"+tc.region {
				t.Fatalf("refreshToken = %q, want rt-%s", stored.Auth.RefreshToken, tc.region)
			}
			if stored.Account.UID != "uid-"+tc.region {
				t.Fatalf("uid = %q, want uid-%s", stored.Account.UID, tc.region)
			}
			// expiresIn must be honoured: a zero expiry is what triggers a
			// refresh storm on the first request.
			if stored.Auth.ExpiresAt == 0 {
				t.Fatal("expiresAt = 0, want a value derived from expiresIn")
			}

			issuer, other := intl, cn
			if tc.region == oauthRegionCN {
				issuer, other = cn, intl
			}
			paths := issuer.servedPaths()
			if len(paths) != 3 {
				t.Fatalf("issuer paths = %v, want state + token + account", paths)
			}
			if !strings.HasPrefix(paths[1], "/v2/plugin/auth/token?state=state-") {
				t.Fatalf("second call = %q, want the token poll", paths[1])
			}
			if !strings.HasPrefix(paths[2], "/v2/plugin/login/account?state=state-") {
				t.Fatalf("third call = %q, want the account poll", paths[2])
			}
			if otherPaths := other.servedPaths(); len(otherPaths) != 0 {
				t.Fatalf("the other realm's issuer was contacted: %v", otherPaths)
			}
		})
	}
}

// TestLoginHeadersCarryTheIssuerOrigin pins the header rule: the state request
// and the polls must present the Origin/Referer of the realm that issued them.
// commonHeaders() hardcodes the CN pair, so an Intl request would otherwise look
// like it came from codebuddy.cn.
func TestLoginHeadersCarryTheIssuerOrigin(t *testing.T) {
	intl := loginEndpointsForRegion(oauthRegionIntl)
	cn := loginEndpointsForRegion(oauthRegionCN)

	cases := []struct {
		name   string
		ep     loginEndpoints
		want   string
		region string
	}{
		{"intl", intl, originRefererGlobal, oauthRegionIntl},
		{"cn", cn, originReferer, oauthRegionCN},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(http.MethodPost, "https://example.invalid/", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		loginHeaders(req, tc.ep)

		if got := req.Header.Get("Origin"); got != tc.want {
			t.Errorf("%s: Origin = %q, want %q", tc.name, got, tc.want)
		}
		if got := req.Header.Get("Referer"); got != tc.want+"/" {
			t.Errorf("%s: Referer = %q, want %q", tc.name, got, tc.want+"/")
		}
		if got := req.Header.Get("User-Agent"); got != clientUA {
			t.Errorf("%s: User-Agent = %q, want %q", tc.name, got, clientUA)
		}
		if got := tc.ep.region; got != tc.region {
			t.Errorf("%s: region = %q, want %q", tc.name, got, tc.region)
		}
	}
}

// TestLoginEndpointsForRegionFallsBackToIntl pins the safety rule: an empty or
// unrecognised region must never resolve back to the CN issuer, because CN
// issuance is exactly what makes an Intl login impossible.
func TestLoginEndpointsForRegionFallsBackToIntl(t *testing.T) {
	for _, region := range []string{"", "??", "global", "international"} {
		ep := loginEndpointsForRegion(region)
		if ep.region != oauthRegionIntl {
			t.Errorf("loginEndpointsForRegion(%q).region = %q, want %q", region, ep.region, oauthRegionIntl)
		}
		if !strings.HasPrefix(ep.authState, upstreamBaseGlobal+"/v2/plugin/auth/state") {
			t.Errorf("loginEndpointsForRegion(%q).authState = %q, want the Global issuer", region, ep.authState)
		}
		if !strings.HasSuffix(ep.authState, "platform=ide") {
			t.Errorf("loginEndpointsForRegion(%q).authState = %q, want platform=ide", region, ep.authState)
		}
	}
	if ep := loginEndpointsForRegion(oauthRegionCN); !strings.HasPrefix(ep.authState, upstreamBaseCN+"/v2/plugin/auth/state") {
		t.Errorf("CN authState = %q, want the CN issuer", ep.authState)
	}
}

// TestNormalizeOAuthRegion covers the accepted spellings and the fallback.
func TestNormalizeOAuthRegion(t *testing.T) {
	cases := map[string]string{
		"":              oauthRegionIntl,
		"intl":          oauthRegionIntl,
		"INTL":          oauthRegionIntl,
		" global ":      oauthRegionIntl,
		"international": oauthRegionIntl,
		"bogus":         oauthRegionIntl,
		"cn":            oauthRegionCN,
		"CN":            oauthRegionCN,
		" cn ":          oauthRegionCN,
		"china":         oauthRegionCN,
		"tencent":       oauthRegionCN,
		`"cn"`:          oauthRegionCN,
	}
	for in, want := range cases {
		if got := normalizeOAuthRegion(in); got != want {
			t.Errorf("normalizeOAuthRegion(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestConfigureOAuthRegion checks the config.yaml → runtime path, the env
// fallback, and that a junk value cannot select CN.
func TestConfigureOAuthRegion(t *testing.T) {
	restore := oauthRegionValue()
	t.Cleanup(func() { _, _ = setOAuthRegionRuntime(restore) })

	// WORKBUDDY_DATA_DIR keeps configure()'s state-file work out of the real
	// plugin data dir; usage_report_url in the YAML skips the endpoint probe
	// (which would otherwise cost two network timeouts per call).
	t.Setenv("WORKBUDDY_DATA_DIR", t.TempDir())

	configureYAML := func(t *testing.T, yaml string) {
		t.Helper()
		raw, err := json.Marshal(struct {
			ConfigYAML []byte `json:"config_yaml"`
		}{ConfigYAML: []byte("usage_report_url: http://127.0.0.1:9/usage\nusage_report_key: test\n" + yaml)})
		if err != nil {
			t.Fatalf("marshal configure request: %v", err)
		}
		configure(raw)
	}

	// config_yaml selects the region.
	t.Setenv("WB_OAUTH_REGION", "")
	configureYAML(t, "oauth_region: cn\n")
	if got := oauthRegionValue(); got != oauthRegionCN {
		t.Fatalf("oauth_region: cn → %q, want %q", got, oauthRegionCN)
	}

	// config_yaml wins over the env fallback.
	t.Setenv("WB_OAUTH_REGION", "cn")
	configureYAML(t, "oauth_region: intl\n")
	if got := oauthRegionValue(); got != oauthRegionIntl {
		t.Fatalf("oauth_region: intl → %q, want %q", got, oauthRegionIntl)
	}

	// No config value → env.
	configureYAML(t, "token_keepalive: true\n")
	if got := oauthRegionValue(); got != oauthRegionCN {
		t.Fatalf("WB_OAUTH_REGION=cn → %q, want %q", got, oauthRegionCN)
	}

	// Junk must not land on CN.
	t.Setenv("WB_OAUTH_REGION", "mars")
	configureYAML(t, "oauth_region: also-mars\n")
	if got := oauthRegionValue(); got != oauthRegionIntl {
		t.Fatalf("junk region → %q, want %q", got, oauthRegionIntl)
	}
}

// TestOAuthRegionRoute exercises the panel's write path end to end: a bad region
// is rejected with 400 and leaves the region untouched, a good one applies.
func TestOAuthRegionRoute(t *testing.T) {
	restoreRegion := oauthRegionValue()
	t.Cleanup(func() { _, _ = setOAuthRegionRuntime(restoreRegion) })
	_, _ = setOAuthRegionRuntime(oauthRegionIntl)

	// This test is about routing, not auth: drop any management key another
	// test may have configured and restore it afterwards.
	managementAPIKeyMu.Lock()
	origKey := managementAPIKey
	managementAPIKey = ""
	managementAPIKeyMu.Unlock()
	t.Cleanup(func() {
		managementAPIKeyMu.Lock()
		managementAPIKey = origKey
		managementAPIKeyMu.Unlock()
	})

	call := func(t *testing.T, body string) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(pluginapi.ManagementRequest{
			Method: http.MethodPost,
			Path:   loadedManagementBasePath() + "/plugins/" + pluginRouteID + "/oauth/region",
			Body:   []byte(body),
		})
		if err != nil {
			t.Fatalf("marshal management request: %v", err)
		}
		out, err := handleManagement(raw)
		if err != nil {
			t.Fatalf("handle management: %v", err)
		}
		var env envelope
		if err := json.Unmarshal(out, &env); err != nil {
			t.Fatalf("decode envelope: %v (raw=%s)", err, out)
		}
		if !env.OK {
			t.Fatalf("envelope reported failure: %s", out)
		}
		var resp pluginapi.ManagementResponse
		if err := json.Unmarshal(env.Result, &resp); err != nil {
			t.Fatalf("decode management response: %v (raw=%s)", err, env.Result)
		}
		payload := map[string]any{}
		if err := json.Unmarshal(resp.Body, &payload); err != nil {
			t.Fatalf("decode management body: %v (raw=%s)", err, resp.Body)
		}
		return resp.StatusCode, payload
	}

	if status, payload := call(t, `{"region":"mars"}`); status != http.StatusBadRequest {
		t.Fatalf("unknown region status = %d, want 400 (body: %v)", status, payload)
	}
	if got := oauthRegionValue(); got != oauthRegionIntl {
		t.Fatalf("region after a rejected write = %q, want %q", got, oauthRegionIntl)
	}

	if status, payload := call(t, `{"region":"cn"}`); status != http.StatusOK {
		t.Fatalf("valid region status = %d, want 200 (body: %v)", status, payload)
	}
	if got := oauthRegionValue(); got != oauthRegionCN {
		t.Fatalf("region after write = %q, want %q", got, oauthRegionCN)
	}

	// An empty body reads the current value without changing it.
	if _, payload := call(t, `{}`); payload["oauth_region"] != oauthRegionCN {
		t.Fatalf("read-back payload = %v, want oauth_region=%q", payload, oauthRegionCN)
	}
}
