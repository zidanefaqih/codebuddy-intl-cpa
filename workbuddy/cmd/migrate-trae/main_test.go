package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationPreservesSettingsAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auth")
	_ = os.Mkdir(authDir, 0700)
	original := []byte(`{"type":"trae","disabled":true,"excluded-models":["x"],"custom":9007199254740993,"auth":{"accessToken":"fixture"}}`)
	cred := filepath.Join(authDir, "trae-fixture.json")
	_ = os.WriteFile(cred, original, 0600)
	config := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(config, []byte("plugins:\n  configs:\n    workbuddy:\n      daily_bonus: false\n    trae:\n      checkin_auto: false\n      callback_port: 0\n      enabled: true\n"), 0600)
	r, err := migrate(authDir, config, "", false)
	if err != nil || r.Applied || r.Accounts != 1 {
		t.Fatalf("dry run failed: %v", err)
	}
	unchanged, _ := os.ReadFile(cred)
	if string(unchanged) != string(original) {
		t.Fatal("dry run changed credential")
	}
	r, err = migrate(authDir, config, filepath.Join(dir, "backup"), true)
	if err != nil || !r.Applied {
		t.Fatal(err)
	}
	stored, _ := os.ReadFile(cred)
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(stored, &doc)
	var excluded []string
	_ = json.Unmarshal(doc["excluded-models"], &excluded)
	if string(doc["type"]) != `"workbuddy"` || string(doc["custom"]) != "9007199254740993" || string(doc["disabled"]) != "true" || len(excluded) != 1 || excluded[0] != "x" {
		t.Fatal("migration damaged settings")
	}
	backup, _ := os.ReadFile(r.Changes[0].Backup)
	if string(backup) != string(original) {
		t.Fatal("backup is not original")
	}
	cfg, _ := os.ReadFile(config)
	if !strings.Contains(string(cfg), "trae_checkin_auto: false") || !strings.Contains(string(cfg), "daily_bonus: false") || !strings.Contains(string(cfg), "trae_callback_port: 0") {
		t.Fatal("config settings lost")
	}
	r, err = migrate(authDir, config, "", false)
	if err != nil || r.Accounts != 0 {
		t.Fatal("repeat migration is not idempotent")
	}
}

func TestMigrationRejectsWrongChannel(t *testing.T) {
	for _, raw := range []string{`{"type":"workbuddy"}`, `{"type":"trae","channel":"other"}`, `null`} {
		if _, _, err := migrateCredential([]byte(raw)); err == nil {
			t.Fatal("invalid migration accepted")
		}
	}
}
