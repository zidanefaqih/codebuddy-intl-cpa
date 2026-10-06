// authfile.go owns every physical auth-file path the plugin touches: the
// workbuddy-<uid>.json naming rule, UID sanitization (path-traversal defense),
// path safety checks, and the read / write / delete helpers that talk to the
// host's auth store via host.auth.* RPC. Callers above (lifecycle reconcile)
// decide when to disable / re-enable / delete; this file decides how.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// authFileNameFor matches toAuthData naming: always workbuddy-<uid>.json when UID is known.
// Bare "workbuddy.json" is legacy single-account only (no UID).
var unsafeUIDChars = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func sanitizeUIDForFileName(uid string) string {
	uid = strings.TrimSpace(uid)
	uid = unsafeUIDChars.ReplaceAllString(uid, "_")
	if uid == "" || uid == "." || uid == ".." {
		return ""
	}
	if len(uid) > 64 {
		uid = uid[:64]
	}
	return uid
}

func authFileNameFor(sa *storedAuth) string {
	if sa != nil {
		if uid := sanitizeUIDForFileName(sa.Account.UID); uid != "" {
			return "workbuddy-" + uid + ".json"
		}
	}
	return authFileName
}

// isLegacyWorkbuddyAuthName reports the historical single-file name that collides
// with multi-account workbuddy-<uid>.json for the same credential.

func isLegacyWorkbuddyAuthName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), authFileName)
}

// isWorkbuddyAuthName accepts both the current per-account filename and the
// historical single-account filename. Host auth-list entries should never be
// matched by a loose prefix alone: that would admit workbuddy-*.txt and empty
// workbuddy- entries into scheduling.
func isWorkbuddyAuthName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `/\\`) {
		return false
	}
	lower := strings.ToLower(name)
	if lower == authFileName {
		return true
	}
	return strings.HasPrefix(lower, providerName+"-") &&
		strings.HasSuffix(lower, ".json") &&
		len(lower) > len(providerName)+len("-.json")
}

// resolveAuthFileTarget picks the canonical file name + path for save/delete.
// Prefer workbuddy-<uid>.json; if the host still points at legacy workbuddy.json
// for a UID-bearing account, rewrite to the uid name and schedule legacy removal.

func resolveAuthFileTarget(sa *storedAuth, phys *hostAuthPhysical) (name, path string, legacyPath string) {
	name = authFileNameFor(sa)
	if phys != nil {
		path = strings.TrimSpace(phys.Path)
		physName := strings.TrimSpace(phys.Name)
		if physName != "" && !isLegacyWorkbuddyAuthName(physName) {
			// Already on multi-account name — keep host name (should match uid form).
			name = physName
		}
		if isLegacyWorkbuddyAuthName(physName) || isLegacyWorkbuddyAuthName(filepath.Base(path)) {
			if sa != nil && strings.TrimSpace(sa.Account.UID) != "" {
				// Migrate: write canonical, delete legacy path after save.
				legacyPath = path
				if isLegacyWorkbuddyAuthName(filepath.Base(path)) {
					// path stays legacy until we write canonical beside it
				}
				// After persist to name, remove legacyPath if different.
			}
		}
	}
	return name, path, legacyPath
}

// hostAuthPersist saves via host API only. Dual-writing the physical path after
// a successful host.auth.save is redundant (host already WriteFile) and can
// re-fire the watcher → extra re-parse / transient dual registration risk.

type hostAuthPhysical struct {
	AuthIndex string
	Name      string
	Path      string
	JSON      []byte
	Disabled  bool
}

func hostAuthGetPhysical(authIndex string) (*hostAuthPhysical, error) {
	body, _ := json.Marshal(map[string]string{"auth_index": authIndex})
	raw, err := hostCall(pluginabi.MethodHostAuthGet, body)
	if err != nil {
		return nil, err
	}
	result, err := hostBridgeUnwrap(raw, pluginabi.MethodHostAuthGet)
	if err != nil {
		return nil, err
	}
	var resp rpcHostAuthGetResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("%s: decode result: %w", pluginabi.MethodHostAuthGet, err)
	}
	return &hostAuthPhysical{
		AuthIndex: resp.AuthIndex,
		Name:      resp.Name,
		Path:      resp.Path,
		JSON:      resp.JSON,
		Disabled:  parseDisabledFromAuthJSON(resp.JSON),
	}, nil
}

// hostAuthSaveJSON persists credential JSON via host.auth.save.

func hostAuthPersist(name, path string, raw []byte) error {
	_ = path // reserved for callers that still pass physical path for migrate logic
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("empty auth file name")
	}
	return hostAuthSaveJSON(name, raw)
}

// hostAuthPersistMigrate is like hostAuthPersist but also removes a legacy path
// when the canonical name differs (workbuddy.json → workbuddy-<uid>.json).

func hostAuthPersistMigrate(name, path, legacyPath string, raw []byte) error {
	if err := hostAuthPersist(name, path, raw); err != nil {
		return err
	}
	// If path was legacy and name is canonical, also write canonical path next to it.
	if legacyPath != "" && !strings.EqualFold(filepath.Base(legacyPath), name) {
		// host.auth.save already wrote name under auth dir; drop legacy file.
		// A-36: use deleteAuthFileInDir (abs path + dir confine) for consistency.
		if isLegacyWorkbuddyAuthName(filepath.Base(legacyPath)) {
			_ = deleteAuthFileInDir(legacyPath, filepath.Dir(legacyPath))
		}
	}
	// If path points at legacy but name is uid form, do not dual-write path (would keep legacy alive).
	return nil
}

// buildAuthFileJSON produces host-save payload: nested storage + top-level metadata.
// extra merges additional top-level keys (optional).

func hostAuthSaveJSON(name string, raw []byte) error {
	_, err := hostAuthSaveJSONResponse(name, raw)
	return err
}

func hostAuthSaveJSONResponse(name string, raw []byte) (pluginapi.HostAuthSaveResponse, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return pluginapi.HostAuthSaveResponse{}, fmt.Errorf("empty auth file name")
	}
	saveReq := pluginapi.HostAuthSaveRequest{
		Name: name,
		JSON: raw,
	}
	saveBody, _ := json.Marshal(saveReq)
	rawResp, err := hostCall(pluginabi.MethodHostAuthSave, saveBody)
	if err != nil {
		return pluginapi.HostAuthSaveResponse{}, fmt.Errorf("host.auth.save: %w", err)
	}
	result, err := hostBridgeUnwrap(rawResp, pluginabi.MethodHostAuthSave)
	if err != nil {
		return pluginapi.HostAuthSaveResponse{}, err
	}
	var saveResp pluginapi.HostAuthSaveResponse
	if err := json.Unmarshal(result, &saveResp); err != nil {
		return pluginapi.HostAuthSaveResponse{}, fmt.Errorf("%s: decode result: %w", pluginabi.MethodHostAuthSave, err)
	}
	return saveResp, nil
}

// mergeAuthStorageJSON replaces only the provider-owned auth/account payloads
// while preserving host metadata such as disabled, note, logo, and unknown
// fields. Token refresh must not silently re-enable an exhausted account or
// strip the metadata that makes it identifiable in the host panel. When sa was
// parsed from a richer source document, unknown provider fields from that
// source are carried forward too.
func mergeAuthStorageJSON(raw []byte, sa *storedAuth) ([]byte, error) {
	if sa == nil {
		return nil, fmt.Errorf("nil storedAuth")
	}
	doc := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		if doc == nil {
			doc = map[string]json.RawMessage{}
		}
	}
	// The parsed source may contain provider fields not represented by
	// storedAuth. Add missing top-level source fields without allowing a stale
	// source snapshot to overwrite metadata from the current physical document.
	var source map[string]json.RawMessage
	if len(strings.TrimSpace(string(sa.rawJSON))) > 0 && json.Unmarshal(sa.rawJSON, &source) == nil && source != nil {
		if len(doc) == 0 {
			doc = source
		} else {
			_, currentNested := doc["auth"]
			for key, value := range source {
				if key == "auth" || key == "account" || (currentNested && isFlatAuthField(key)) {
					continue
				}
				if _, exists := doc[key]; !exists {
					doc[key] = value
				}
			}
		}
	}
	// A nested import replacing a legacy flat file is canonicalized to nested
	// storage so provider-specific fields inside source.auth/source.account are
	// not discarded. Existing flat files parsed from disk have a flat source too,
	// so their shape remains unchanged.
	if _, currentNested := doc["auth"]; !currentNested {
		if sourceAuth, sourceNested := source["auth"]; sourceNested {
			doc["auth"] = sourceAuth
			if sourceAccount, ok := source["account"]; ok {
				doc["account"] = sourceAccount
			}
			for key := range doc {
				if isFlatAuthField(key) {
					delete(doc, key)
				}
			}
		}
	}
	storage, err := json.Marshal(sa)
	if err != nil {
		return nil, err
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(storage, &nested); err != nil {
		return nil, err
	}
	if _, nestedShape := doc["auth"]; nestedShape {
		// Keep unknown provider fields inside the nested objects too. Prefer the
		// current physical document over a stale parsed source, then overlay the
		// typed fields that this mutation intentionally changed.
		if source != nil {
			if sourceAuth, ok := source["auth"]; ok {
				doc["auth"], err = mergeJSONObjects(sourceAuth, doc["auth"])
				if err != nil {
					return nil, err
				}
			}
			if sourceAccount, ok := source["account"]; ok {
				doc["account"], err = mergeJSONObjects(sourceAccount, doc["account"])
				if err != nil {
					return nil, err
				}
			}
		}
		doc["auth"], err = mergeJSONObjects(doc["auth"], nested["auth"])
		if err != nil {
			return nil, err
		}
		doc["account"], err = mergeJSONObjects(doc["account"], nested["account"])
		if err != nil {
			return nil, err
		}
	} else {
		// Preserve flat legacy files as flat files. The host accepts both
		// shapes, and retaining the shape avoids leaving stale duplicate token
		// fields beside the newly refreshed nested payload.
		set := func(key string, value any) error {
			encoded, err := json.Marshal(value)
			if err != nil {
				return err
			}
			doc[key] = encoded
			return nil
		}
		for key, value := range map[string]any{
			"accessToken":  sa.Auth.AccessToken,
			"refreshToken": sa.Auth.RefreshToken,
			"expiresAt":    sa.Auth.ExpiresAt,
			"domain":       sa.Auth.Domain,
			"uid":          sa.Account.UID,
			"enterpriseId": sa.Account.EnterpriseID,
			"nickname":     sa.Account.Nickname,
		} {
			if err := set(key, value); err != nil {
				return nil, err
			}
		}
	}
	if _, ok := doc["type"]; !ok {
		doc["type"] = json.RawMessage(`"` + providerName + `"`)
	}
	if _, ok := doc["provider"]; !ok {
		doc["provider"] = json.RawMessage(`"` + providerName + `"`)
	}
	return json.Marshal(doc)
}

func isFlatAuthField(key string) bool {
	switch key {
	case "accessToken", "refreshToken", "expiresAt", "domain", "uid", "enterpriseId", "nickname":
		return true
	default:
		return false
	}
}

// storageJSONForAuth returns provider storage for AuthData without dropping
// fields that were present in the source auth file. Parsed files carry their
// original JSON; newly-created login records fall back to the typed shape.
func storageJSONForAuth(sa *storedAuth) []byte {
	if sa == nil {
		return nil
	}
	if len(sa.rawJSON) > 0 {
		if merged, err := mergeAuthStorageJSON(sa.rawJSON, sa); err == nil {
			return merged
		}
		return append([]byte(nil), sa.rawJSON...)
	}
	raw, _ := json.Marshal(sa)
	return raw
}

// mergeJSONObjects overlays replacement fields onto an existing JSON object
// while preserving fields unknown to the current Go structs. If the existing
// value is not an object, the replacement object is used as-is.
func mergeJSONObjects(existing, replacement json.RawMessage) (json.RawMessage, error) {
	var out map[string]json.RawMessage
	if len(existing) > 0 && string(existing) != "null" {
		if err := json.Unmarshal(existing, &out); err != nil {
			out = nil
		}
	}
	if out == nil {
		out = make(map[string]json.RawMessage)
	}
	var next map[string]json.RawMessage
	if err := json.Unmarshal(replacement, &next); err != nil {
		return nil, err
	}
	for key, value := range next {
		out[key] = value
	}
	return json.Marshal(out)
}

// authMetadataFromJSON extracts host-managed top-level metadata without
// copying provider-owned flat credential fields into AuthData.Metadata.
func authMetadataFromJSON(raw []byte) map[string]any {
	var doc map[string]any
	if len(strings.TrimSpace(string(raw))) == 0 || json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	out := make(map[string]any)
	for key, value := range doc {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "auth", "account", "accesstoken", "refreshtoken", "expiresat", "domain", "uid", "enterpriseid", "nickname":
			continue
		default:
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func metadataBool(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true") || strings.TrimSpace(v) == "1"
	case float64:
		return v != 0
	default:
		return false
	}
}

// mergeAuthMetadata keeps host-owned values supplied by CPA while filling in
// provider defaults. The disabled bit is normalized because it controls both
// host scheduling and the persisted top-level auth state.
func mergeAuthMetadata(sa *storedAuth, existing map[string]any, disabled bool) map[string]any {
	defaults := enrichAuthMetadata(sa, nil, disabled)
	out := make(map[string]any, len(existing)+len(defaults))
	for key, value := range existing {
		out[key] = value
	}
	for key, value := range defaults {
		if _, ok := out[key]; !ok {
			out[key] = value
		}
	}
	out["type"] = providerName
	out["provider"] = providerName
	out["disabled"] = disabled
	return out
}

// authForPhysicalSave prefers the provider payload currently on disk over a
// possibly stale caller snapshot. Lifecycle reconciliation may spend time
// fetching credits while a token refresh updates the same auth file.
func authForPhysicalSave(phys *hostAuthPhysical, fallback *storedAuth) *storedAuth {
	if phys != nil && len(phys.JSON) > 0 {
		if current, err := parseStored(phys.JSON); err == nil {
			return current
		}
	}
	return fallback
}

// buildAuthFileJSONPreserving updates lifecycle metadata on the current
// physical payload while retaining the current tokens and unknown host fields.
// The caller holds the per-account mutation lock, so the physical snapshot is
// stable for the save operation.
func buildAuthFileJSONPreserving(phys *hostAuthPhysical, sa *storedAuth, disabled bool, note string, extra map[string]any) ([]byte, error) {
	if phys == nil || len(phys.JSON) == 0 {
		return buildAuthFileJSON(sa, disabled, note, extra)
	}
	sa = authForPhysicalSave(phys, sa)
	raw, err := mergeAuthStorageJSON(phys.JSON, sa)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	doc["type"] = providerName
	doc["provider"] = providerName
	doc["logo"] = pluginLogoURL
	doc["disabled"] = disabled
	doc["note"] = note
	for key, value := range extra {
		doc[key] = value
	}
	return json.Marshal(doc)
}

// lifecycleStateUnchanged avoids redundant saves when note/disabled unchanged.

func buildAuthFileJSON(sa *storedAuth, disabled bool, note string, extra map[string]any) ([]byte, error) {
	if sa == nil {
		return nil, fmt.Errorf("nil storedAuth")
	}
	var doc map[string]any
	if len(sa.rawJSON) > 0 {
		raw, err := mergeAuthStorageJSON(sa.rawJSON, sa)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
	} else {
		storage, err := json.Marshal(sa)
		if err != nil {
			return nil, err
		}
		var nested map[string]any
		if err := json.Unmarshal(storage, &nested); err != nil {
			return nil, err
		}
		doc = map[string]any{
			"auth":    nested["auth"],
			"account": nested["account"],
		}
	}
	doc["type"] = providerName
	doc["provider"] = providerName
	doc["logo"] = pluginLogoURL
	doc["disabled"] = disabled
	doc["note"] = note
	for k, v := range extra {
		doc[k] = v
	}
	return json.Marshal(doc)
}

// parseDisabledFromAuthJSON reads top-level disabled from physical auth JSON.

func parseDisabledFromAuthJSON(raw []byte) bool {
	var m struct {
		Disabled bool `json:"disabled"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Disabled
}

// isSafeWorkbuddyAuthPath rejects non-workbuddy filenames, empty paths, and
// traversal attempts. It validates both the basename pattern AND that the path
// does not escape via ".." segments. Callers that need to confine deletes to
// a specific directory should additionally check isPathUnder(path, dir).

func isSafeWorkbuddyAuthPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	// Reject any path containing ".." — prevents traversal regardless of basename.
	if strings.Contains(filepath.ToSlash(path), "../") || strings.Contains(filepath.ToSlash(path), "/..") {
		return false
	}
	base := filepath.Base(path)
	if !isWorkbuddyAuthName(base) {
		return false
	}
	// Path traversal / absolute weirdness: base must equal cleaned base.
	if base != filepath.Base(filepath.Clean(path)) {
		return false
	}
	return true
}

// isPathUnder reports whether path is inside dir (after cleaning both).
// Empty dir means "no constraint" (returns true for any safe path).

func isPathUnder(path, dir string) bool {
	path = strings.TrimSpace(path)
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return true
	}
	cleanPath := filepath.Clean(path)
	cleanDir := filepath.Clean(dir)
	if cleanPath == cleanDir {
		return false // path is the dir itself, not under it
	}
	rel, err := filepath.Rel(cleanDir, cleanPath)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..") && !strings.Contains(rel, string(filepath.Separator)+"..")
}

// deleteAuthFileAt removes a workbuddy auth file. Missing file is success.
// Deprecated: use deleteAuthFileInDir instead (adds directory + absolute path
// confinement). Retained for test coverage of the base safe-delete path.

func deleteAuthFileAt(path string) error {
	if !isSafeWorkbuddyAuthPath(path) {
		return fmt.Errorf("refusing to delete unsafe path: %s", path)
	}
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// deleteAuthFileInDir is like deleteAuthFileAt but additionally requires the
// path to be under dir. Use for lifecycle deletes where the auth directory is
// known — prevents a malicious/buggy host path from deleting arbitrary files.
// The path MUST be absolute (defense against relative-path CWD deletion).

func deleteAuthFileInDir(path, dir string) error {
	if !isSafeWorkbuddyAuthPath(path) {
		return fmt.Errorf("refusing to delete unsafe path: %s", path)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("refusing to delete relative path: %s", path)
	}
	if dir != "" && !isPathUnder(path, dir) {
		return fmt.Errorf("refusing to delete path outside auth dir: %s (dir=%s)", path, dir)
	}
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// hostAuthGetFull returns physical JSON, path, and name for an auth index.
