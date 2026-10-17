# CodeBuddy + Trae — Windows unified build

Version: `0.9.7+unified-trae.0.1.6`. CPA SDK: **8.0.23**. Local integration
branch: `codex/unified-trae-codebuddy`. Includes the SSE/Responses compatibility
changes proposed in upstream PR #9.

## 使用

- 一个 DLL、一个 **CodeBuddy + Trae** 菜单，管理 CodeBuddy 国际版、国内版和
  Trae 国内版 SOLO。支持中文 / English 和深色模式。
- CodeBuddy 页保留原有管理功能；Trae 页提供网页登录、JSON 导入、账号开关、
  签到、余额和每账号模型开关。总览和用量页显示各渠道。
- Trae 模型改用 **`trae/<模型 ID>`**，客户端需要重新选择。凭证自定义 `prefix`
  仍由 CPA 加在最前面。CodeBuddy 模型命名和配置保持兼容。
- 账号池、刷新任务、模型缓存和积分统计独立。两种用量日志分别为
  `.codebuddy_data/request-usage*.jsonl` 和 `trae-request-usage*.jsonl`；
  `WORKBUDDY_DATA_DIR` 可覆盖目录。
- Trae 只支持国内版 SOLO，不支持 Work / `-official` 模型；凭证级 `proxy_url`
  不受支持，使用 CPA 全局代理。

## 构建与安装

需要 Go 1.26 和 Windows amd64 GCC：

```powershell
Set-Location workbuddy
go test -race -count=1 ./...
go vet ./...
go build -buildmode=c-shared -o dist/windows/amd64/workbuddy.dll .
go build -o dist/windows/amd64/migrate-trae.exe ./cmd/migrate-trae
Set-Location ..
./scripts/install-unified.ps1 -CPAHome 'C:\path\to\CLIProxyAPI' -AuthDir (Join-Path $env:USERPROFILE '.cli-proxy-api')
```

安装器会停止对应路径的 CPA，备份配置、原 DLL 和待迁移凭证，迁移 Trae 配置与
凭证，移走单独的 `trae.dll` 并替换原 workbuddy DLL。原先运行中的 CPA 会重新启动；
原先停止的保持停止，可加 `-StartCPA` 启动。安装失败时恢复备份。

Trae 文件名仍为 `trae-<uid>.json`，提供商改为 `type: workbuddy`，增加
`channel: trae`；其余字段和大整数值保留。旧 JSON 也可从 Trae 页导入。
Trae 配置迁入 `plugins.configs.workbuddy.trae_*`，包括 `trae_checkin_auto`、
`trae_checkin_time`、`trae_token_keepalive`、`trae_callback_port`、`trae_max_attempts`。

备份位于 CPA 的 `backups/unified-*`。手动回退前停止 CPA，按
`migration/manifest.json` 恢复对应备份文件，再恢复原 workbuddy DLL 和 trae DLL。
备份包含凭证，请和原凭证目录一样保管。

本地 `+unified-trae.*` 构建使用此安装器更新；CodeBuddy 单渠道更新包会覆盖整合功能。
该本地构建的更新提示显示当前整合版本。上游采用正式版本号后，恢复原有更新检查。

## Verification

- Go regression/race tests and `go vet ./...`.
- `scripts/test-unified-panel.cjs`: Playwright browser tests for bilingual tabs,
  the embedded CodeBuddy UI, model drafts/save, import, pending operations,
  authentication rejection, dark and mobile layouts. Uses `NODE_PATH`.
- `scripts/smoke-unified.ps1`: isolated CPA 8.0.23 with empty auth directory,
  one DLL, namespaced config, overview, resources and authentication.
- Compile `scripts/abi-unified-smoke.c` to
  `workbuddy/dist/windows/amd64/abi-unified-smoke.exe`, then run
  `scripts/test-unified-abi.cjs`: a real Windows DLL parses both providers,
  discovers Trae models, completes and streams both protocols, and rejects
  crossed model/credential requests. HTTP callbacks use synthetic fixtures.

Trae source and tests are embedded under `workbuddy/internal/trae`, with its
MIT license and attribution retained. CodeBuddy retains its original license
and contributors.
