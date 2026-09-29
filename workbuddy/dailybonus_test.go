package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// envelope wraps an upstream payload the way the growth endpoints do.
func growthEnvelope(data string) string {
	return `{"code":0,"msg":"OK","data":` + data + `}`
}

func TestGrowthClientToken(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 64; i++ {
		tok := growthClientToken()
		if seen[tok] {
			t.Fatalf("duplicate token generated: %s", tok)
		}
		seen[tok] = true
		parts := strings.Split(tok, "-")
		if len(parts) != 5 {
			t.Fatalf("token %q is not UUID-shaped (got %d groups)", tok, len(parts))
		}
		want := []int{8, 4, 4, 4, 12}
		for i, p := range parts {
			if len(p) != want[i] {
				t.Fatalf("token %q group %d has length %d, want %d", tok, i, len(p), want[i])
			}
		}
	}
}

func TestDailyBonusSchedule(t *testing.T) {
	// Every configured hour must open its own [h, h+1) window.
	for _, h := range dailyBonusHours {
		at := time.Date(2026, 9, 29, h, 0, 0, 0, time.Local)
		if !shouldRunDailyBonusNow(at) {
			t.Errorf("hour %d: window not open at the top of the hour", h)
		}
		inside := time.Date(2026, 9, 29, h, 59, 0, 0, time.Local)
		if !shouldRunDailyBonusNow(inside) {
			t.Errorf("hour %d: window should still be open at %02d:59", h, h)
		}
	}

	// A moment outside every window must not fire.
	outside := time.Date(2026, 9, 29, 2, 30, 0, 0, time.Local)
	if shouldRunDailyBonusNow(outside) {
		t.Error("02:30 should not open any daily-bonus window")
	}

	// nextDailyBonusTime always moves forward and lands on a configured hour.
	now := time.Date(2026, 9, 29, 9, 30, 0, 0, time.Local)
	next := nextDailyBonusTime(now)
	if !next.After(now) {
		t.Errorf("nextDailyBonusTime(%v) = %v, should be in the future", now, next)
	}
	found := false
	for _, h := range dailyBonusHours {
		if next.Hour() == h {
			found = true
		}
	}
	if !found {
		t.Errorf("nextDailyBonusTime returned hour %d, not one of %v", next.Hour(), dailyBonusHours)
	}
}

// TestFetchGrowthToday covers the "which day is it" rule: the API's own heatmap
// decides, using the last cell, so no local timezone maths is involved.
func TestFetchGrowthToday(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("heatmap must be fetched with GET, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, growthHeatmapPath) {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(growthEnvelope(
			`{"cells":[{"date":"2026-09-27","score":0},{"date":"2026-09-28","score":2},{"date":"2026-09-29","score":0}]}`)))
	}))
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	// Empty domain → CN path → billingBase, which the test server replaced.
	today, lit, err := fetchGrowthToday(&storedAuth{})
	if err != nil {
		t.Fatalf("fetchGrowthToday: %v", err)
	}
	if today.Date != "2026-09-29" {
		t.Errorf("today = %q, want the last cell 2026-09-29", today.Date)
	}
	if today.Score != 0 {
		t.Errorf("today score = %d, want 0", today.Score)
	}
	if len(lit) != 1 || lit[0] != "2026-09-28" {
		t.Errorf("lit dates = %v, want [2026-09-28]", lit)
	}
}

// TestGrowthGetRejectsNonJSON guards the error path that used to be impossible
// to diagnose: an APISIX/gateway HTML page (session dead, wrong realm host)
// must say so instead of surfacing a bare "parse failed".
func TestGrowthGetRejectsNonJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("<html><body>401 Unauthorized</body></html>"))
	}))
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	_, err := growthGet(&storedAuth{}, growthHeatmapPath)
	if err == nil {
		t.Fatal("expected an error for an HTML body")
	}
	if !strings.Contains(err.Error(), "parse failed") {
		t.Errorf("error should mention parse failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), "<html>") {
		t.Errorf("error should carry a body snippet for diagnosis, got: %v", err)
	}
}

// TestSendGrowthActivityPayload pins the beacon shape: a single-element array
// whose field names match the upstream contract exactly. A rename here would
// silently stop lighting days.
func TestSendGrowthActivityPayload(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("report must be POSTed, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, growthReportPath) {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("report body is not a JSON array: %v (%s)", err, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	sa := &storedAuth{}
	sa.Account.UID = "13f49b24-783a-4e18-80bb-724e617f35d0"
	if err := sendGrowthActivity(sa); err != nil {
		t.Fatalf("sendGrowthActivity: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("payload has %d events, want exactly 1", len(got))
	}
	ev := got[0]
	want := map[string]any{
		"eventCode":        "chat_request_send",
		"mode":             "craft",
		"agentName":        "default",
		"agentType":        "conversation",
		"userId":           "13f49b24-783a-4e18-80bb-724e617f35d0",
		"inputLength":      float64(growthReportInputLen),
		"requestModelId":   growthReportModelID,
		"requestModelName": growthReportModelName,
	}
	for k, v := range want {
		if ev[k] != v {
			t.Errorf("payload[%q] = %v, want %v", k, ev[k], v)
		}
	}
	// The empty slices must be present, not omitted — upstream reads them.
	for _, k := range []string{"mentionContexts", "knowledgeId", "knowledgeName"} {
		arr, ok := ev[k].([]any)
		if !ok || len(arr) != 0 {
			t.Errorf("payload[%q] = %v, want an empty array", k, ev[k])
		}
	}
	// Ids are caller-generated and must be consistent within the event.
	for _, k := range []string{"conversationId", "requestId", "rootRequestId", "parentConversationId"} {
		s, ok := ev[k].(string)
		if !ok || s == "" {
			t.Errorf("payload[%q] missing", k)
		}
	}
	if ev["conversationId"] != ev["requestId"] || ev["requestId"] != ev["rootRequestId"] {
		t.Errorf("ids should agree: conv=%v req=%v root=%v",
			ev["conversationId"], ev["requestId"], ev["rootRequestId"])
	}
	if ts, ok := ev["timestamp"].(float64); !ok || ts <= 0 {
		t.Errorf("timestamp missing or non-positive: %v", ev["timestamp"])
	}
}

// TestGrowthHeatmapDecode pins the response shape used to decide whether a day
// is already lit.
func TestGrowthHeatmapDecode(t *testing.T) {
	var hm growthHeatmap
	if err := json.Unmarshal([]byte(`{"cells":[{"date":"2026-09-28","score":2,"has_new_buddy":false}]}`), &hm); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(hm.Cells) != 1 || hm.Cells[0].Date != "2026-09-28" || hm.Cells[0].Score != 2 {
		t.Fatalf("unexpected decode result: %+v", hm.Cells)
	}
}
