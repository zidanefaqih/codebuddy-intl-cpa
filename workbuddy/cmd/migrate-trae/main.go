// migrate-trae converts the two-plugin installation to the unified provider.
// It defaults to a dry run; apply while CPA is stopped. Backups contain secrets.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type change struct {
	Target string `json:"target"`
	Backup string `json:"backup"`
	data   []byte
	mode   os.FileMode
}
type report struct {
	Applied   bool     `json:"applied"`
	Accounts  int      `json:"accounts"`
	BackupDir string   `json:"backup_dir,omitempty"`
	Changes   []change `json:"changes"`
}

func main() {
	authDir := flag.String("auth-dir", "", "CPA credential directory")
	config := flag.String("config", "", "CPA config.yaml")
	backup := flag.String("backup-dir", "", "new directory for backups (required with -apply)")
	apply := flag.Bool("apply", false, "write changes after creating backups; CPA must be stopped")
	flag.Parse()
	result, err := migrate(*authDir, *config, *backup, *apply)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	b, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(b))
}

func migrate(authDir, config, backup string, apply bool) (report, error) {
	r := report{Changes: []change{}}
	if authDir == "" || config == "" {
		return r, fmt.Errorf("-auth-dir and -config are required")
	}
	authDir, _ = filepath.Abs(authDir)
	config, _ = filepath.Abs(config)
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return r, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "trae-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return r, fmt.Errorf("refusing symlink credential %s", entry.Name())
		}
		target := filepath.Join(authDir, entry.Name())
		raw, err := os.ReadFile(target)
		if err != nil {
			return r, err
		}
		out, changed, err := migrateCredential(raw)
		if err != nil {
			return r, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if changed {
			r.Changes = append(r.Changes, change{Target: target, data: out, mode: 0600})
			r.Accounts++
		}
	}
	raw, err := os.ReadFile(config)
	if err != nil {
		return r, err
	}
	out, err := migrateConfig(raw)
	if err != nil {
		return r, err
	}
	if !bytes.Equal(out, raw) {
		r.Changes = append(r.Changes, change{Target: config, data: out, mode: 0600})
	}
	if !apply {
		return r, nil
	}
	if backup == "" {
		return r, fmt.Errorf("-backup-dir is required with -apply")
	}
	backup, _ = filepath.Abs(backup)
	if _, err = os.Stat(backup); !os.IsNotExist(err) {
		return r, fmt.Errorf("backup directory must not exist")
	}
	if err = os.MkdirAll(backup, 0700); err != nil {
		return r, err
	}
	r.BackupDir = backup
	// Back up every target before the first mutation, including config.yaml.
	for i := range r.Changes {
		c := &r.Changes[i]
		c.Backup = filepath.Join(backup, fmt.Sprintf("%03d-%s", i, filepath.Base(c.Target)))
		old, err := os.ReadFile(c.Target)
		if err != nil {
			return r, err
		}
		info, err := os.Stat(c.Target)
		if err != nil {
			return r, err
		}
		c.mode = info.Mode().Perm()
		if err = os.WriteFile(c.Backup, old, 0600); err != nil {
			return r, err
		}
	}
	manifest, _ := json.MarshalIndent(r, "", "  ")
	if err = os.WriteFile(filepath.Join(backup, "manifest.json"), manifest, 0600); err != nil {
		return r, err
	}
	for i, c := range r.Changes {
		if err = atomicWrite(c.Target, c.data, c.mode); err != nil {
			var rollbackErr error
			for _, done := range r.Changes[:i+1] {
				old, e := os.ReadFile(done.Backup)
				if e == nil {
					e = atomicWrite(done.Target, old, done.mode)
				}
				if e != nil {
					rollbackErr = e
				}
			}
			return r, fmt.Errorf("migration failed (%v); rollback error: %v", err, rollbackErr)
		}
	}
	r.Applied = true
	return r, nil
}

func atomicWrite(target string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(target), ".unified-migrate-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, target)
}

func migrateCredential(raw []byte) ([]byte, bool, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return nil, false, fmt.Errorf("invalid JSON credential")
	}
	var typ, channel string
	_ = json.Unmarshal(doc["type"], &typ)
	_ = json.Unmarshal(doc["channel"], &channel)
	if typ == "workbuddy" && channel == "trae" {
		return raw, false, nil
	}
	if typ != "trae" {
		return nil, false, fmt.Errorf("unexpected provider; migration stopped")
	}
	if channel != "" && channel != "trae" {
		return nil, false, fmt.Errorf("conflicting channel; migration stopped")
	}
	doc["type"] = json.RawMessage(`"workbuddy"`)
	doc["channel"] = json.RawMessage(`"trae"`)
	if _, ok := doc["provider"]; ok {
		doc["provider"] = json.RawMessage(`"workbuddy"`)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	return append(out, '\n'), true, err
}

func mapping(parent *yaml.Node, key string) *yaml.Node {
	for i := 0; i < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			return parent.Content[i+1]
		}
	}
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"}, n)
	return n
}
func put(parent *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			parent.Content[i+1] = value
			return
		}
	}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}
func boolNode(v bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(v)}
}
func migrateConfig(raw []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("invalid CPA config root")
	}
	plugins := mapping(doc.Content[0], "plugins")
	if plugins.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("invalid plugins configuration")
	}
	configs := mapping(plugins, "configs")
	if configs.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("invalid plugin configs")
	}
	wb := mapping(configs, "workbuddy")
	tr := mapping(configs, "trae")
	if wb.Kind != yaml.MappingNode || tr.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("invalid provider config")
	}
	// Only known Trae options are transferred. Store metadata is not a runtime setting.
	known := map[string]bool{"checkin_auto": true, "checkin_time": true, "token_keepalive": true, "max_attempts": true, "usage_file": true, "callback_port": true}
	for i := 0; i < len(tr.Content); i += 2 {
		key := tr.Content[i].Value
		if !known[key] {
			continue
		}
		exists := false
		for j := 0; j < len(wb.Content); j += 2 {
			if wb.Content[j].Value == "trae_"+key {
				exists = true
			}
		}
		if !exists {
			put(wb, "trae_"+key, tr.Content[i+1])
		}
	}
	put(wb, "enabled", boolNode(true))
	put(tr, "enabled", boolNode(false))
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(4)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
