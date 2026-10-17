# CodeBuddy Intl CPA 简体中文版

简体中文 | [English](README.md)

面向腾讯 **CodeBuddy 国际版（Global / Intl）账号**的 CLIProxyAPI（CPA）插件，包含其他 CPA 插件跳过的**国际账号每日积分奖励**。

面板右上角可选择 **中文 / English**，切换立即生效，并保存在当前浏览器中。首次打开时根据浏览器语言选择。

> **关于命名。** 项目名是 `codebuddy-intl-cpa`，插件内部标识保留 `workbuddy`：文件名 `workbuddy.so`、认证类型 `workbuddy`、凭据文件 `workbuddy-<uid>.json`、配置路径 `plugins.configs.workbuddy`，发行包名也沿用该标识。这些是与宿主和现有凭据约定的名称，改名会导致已有安装和磁盘上的凭据失效。面向人阅读的部分（本 README、商店信息、插件元数据、面板、发行标题）统一使用 **CodeBuddy Intl CPA**。

> **状态：** 已在生产环境运行。31 个 Go 源文件（约 13.6k 行）加 39 个测试文件，以及一个通过 `go:embed` 嵌入的 3,117 行管理面板。

---

## 为什么用这个

目前该服务商的其他 CPA 插件都是**国内优先**：它们把成长中心限制在国内版，假设这套接口在海外并不存在。这个假设是错的，结果是国际账号完全没有每日收入，只有每月 100 积分的免费额度。

本插件做了相反的事：

| | 其他 CPA 插件 | 本插件 |
| --- | --- | --- |
| 成长中心（`/v2/report`、热力图、连续天数） | 仅国内版 | **国内 + 国际** |
| 国际账号每日积分奖励 | ✗ | ✅ **每日 30 积分** |
| 429 / `code 6004` / 积分耗尽的切换重试 | 交给宿主 | **插件内按账号处理** |
| 逐请求积分 / Token 记录 | ✗ | ✅ **JSONL + 面板** |
| 按 realm 的模型目录与前缀 | 硬编码 | ✅ **模型管理** |

---

## 主要功能

### 1. 国际账号每日奖励

国际账号每个账号每天可获得 **30 积分**，无需桌面客户端。

原理：客户端向 `POST /v2/report` 上报一个事件（`eventCode: chat_request_send`），成长中心就把当天计为活跃日。活跃日在 `GET /activity/growth/heatmap` 中表现为非零 `score`，次日起账号会收到一个 **30 积分的奖励包**（`TCACA_code_007_nzdH5h4Nl0`，平台奖励积分，有效期 30 天）。

已在 **2026-09-28 → 2026-09-29** 用 7 个账号验证：仅靠上报点亮的 6 个国际账号都在次日上午收到了 30 积分奖励包，另有 1 个账号是通过客户端普通登录点亮的。验证方法见[自行验证](#自行验证)。

插件按计划执行，并且**按账号按天幂等**——先读热力图，只上报尚未点亮的日期，因此多跑几次不会重复计数。

| | |
| --- | --- |
| 计划 | 插件本地时间的 `08:00 / 12:00 / 16:00 / 20:00`，走插件共用的调度 tick |
| 配置 | `daily_bonus: true`（默认开启） |
| 面板 | **每日奖励（国际）**开关，以及每行的**报告**按钮 |
| 手动触发 | `POST /v0/management/plugins/workbuddy/dailybonus`（带 `{"auth_index":"…"}` 则只跑一个账号） |
| 状态查询 | `GET /v0/management/plugins/workbuddy/dailybonus/status`——按账号返回日期、score、已点亮日期 |

几点需要注意：

- **"今天"以接口热力图为准**（取最后一个格子），不用本地时间，因此不涉及时区换算，中国时区的跨日也无需特殊处理。
- **每天跑四次。** 已点亮的日期会跳过，多余的 tick 没有代价；某个 tick 失败（例如刚开机网络未就绪）由下一个补上。
- **上报请求永不重试。** 该事件在上游按天幂等，但盲目重发会让当天计数翻倍（`score` 2 → 4）。失败的发送等下一个 tick 即可。
- **仅国际账号。** 国内账号继续走签到流程，且国内成长中心在另一台主机（`copilot.tencent.com`）上。
- **计划时间跟随 CPA 进程的时区**，也就是容器的 `TZ`，而不是浏览器所在时区。容器保持 `Asia/Shanghai` 时，这四次 tick 对应 WIB 的 07:00 / 11:00 / 15:00 / 19:00。奖励本身始终按中国时间到账，因为"今天"取自接口热力图，而非本地推算。
- **"全部报告"是限速串行，不是并发轰炸。** 批量按钮逐个账号处理，间隔 1.5 秒，与宿主自带上报脚本的节奏一致。并发轰炸正是之前 429 风暴的来源。
- **每行按钮是诚实的。** 只有在确认当天未点亮时才提供**报告**；点亮后显示**已点亮**并禁用。尚无数据的账号显示 `—`，不会给出未经读取的结论。

**额外收益：** 连续天数还会解锁阶梯奖励（`14 天` → +50 积分，`28 天` → +150 积分），叠加在每日额度之上。

### 2. 国内账号每日签到

国内账号保留自己的奖励路径：每日签到 **100 积分/天**，另有 500/月。开启 `checkin_auto` 时插件在 CPA 进程时区的 **09:00 / 21:00** 自动执行；面板提供每行的**签到**按钮和**全部签到**批量操作。签到按账号串行，两个浏览器标签页不会重复提交。

### 3. 插件内账号切换与准入控制

CPA 内置的冷却与重试只作用于宿主自身的执行器。`c-shared` 插件自己发上游请求，`429` / `code 6004` 不会到达宿主选择器，因此不会发生切换——请求直接失败。

本插件维护按账号的冷却表，在返回错误前用另一个已启用账号重试**同一个请求**（`failover.go`、`retry.go`）。

明确的积分耗尽（HTTP 402、code 14018、`Credits exhausted`）会在任何输出分块之前处理：在同 realm 内重试，并把被拒账号隔离到接口给出的重置时间，或 30 分钟。隔离是进程内的，重启即清除；它在 `lifecycle_auto:false` 下同样有效，不会禁用或删除认证文件。已输出部分内容、空的 HTTP 200 流、传输错误都会停止请求而不重放。切换账号可能导致替换账号上的提示缓存未命中；缓存是否共享随模型和后端而变。

**准入控制。** 每个账号最多 **2 个进行中的请求**，跨模型并按宿主身份（UID + realm）去重。两个槽位都占用时，执行器立即尝试同 realm 的另一个已启用账号；若全部账号都忙，则快速失败，不发出付费的上游请求。**没有本地等待队列**。每个槽位预留后会重新检查隔离状态，因此并发请求不会继续发给刚被 429 拒绝的账号。面板显示的 `当前/上限` 徽标（如 `0/2`）直接来自该准入闸门。

**会话软粘性。** 一个会话在槽位空闲时继续使用上次执行的账号，该账号忙时切到空闲的同级账号，并保留新账号，避免提示缓存被来回弹。粘性键由客户端凭据 + 稳定会话标识 + 模型 + realm 摘要而成；绑定有数量上限、1 小时过期，并持久化到 `session-affinity.json`，因此重启不会搬动会话。

路由始终按会话进行：**面板没有"当前使用账号"这一项**，因为一个全局选择会泄漏到其他会话，并迫使不支持跨账号共享缓存的模型重写提示缓存。

软 429/`code 6004` 隔离粒度是**账号 + 上游模型**；积分耗尽仍是账号级。当某模型的全部账号都在冷却时，请求可在 30 秒后探测其中一个软限流账号，每个账号同一时间只探测一个。新的拒绝会重置退避。插件不会探测已耗尽或已禁用账号、不跨 realm、也不重放部分输出/空响应/传输失败的请求。上游仍可能因 RPM、Token 用量或共享配额拒绝请求——准入控制无法保证上游容量。

不同 CPA 进程的限额各自独立，因此生产与开发环境不要共用同一个账号。切换账号可能让 `gpt-6-sol`、`gpt-6.1-sol` 丢失提示缓存；账号限额并不等于缓存共享保证。

### 4. 逐请求用量与积分记录

流经插件的每个对话请求都会被记录：账号、模型、输入/输出/缓存 Token，以及从终端 SSE 分块取得的上游 `credit` 值。以 JSONL 追加保存在认证目录附近，约 50 MB 轮转（`request-usage-<YYYY-MM>.jsonl`，旧数据保留）。面板的 **CodeBuddy 用量记录**由此驱动。日志不记录任何凭据。

这也是让缓存经济性变得可见的原因：一个 24k Token 的请求在完全命中缓存时约 0.05 积分，而不是约 0.62。

为排查偶发的提示缓存未命中，插件还可额外记录**不含内容的请求哈希**和实际发出的会话 ID。该功能**默认关闭**——需要在插件环境中设置 `WB_CACHE_TRACE=1` 才会采集，因为指纹会占据记账日志的绝大部分（实测每行 99.4%）并拖慢用量页面。字段、隐私边界和对比脚本见[缓存追踪](workbuddy/docs/cache-tracing.md)。按选项的摘要只标识*哪一个*已知设置发生变化，不记录其取值。`scripts/study-cache-misses.py` 会在活动与轮转日志上生成私密的、仅含元数据的报告，区分前缀丢失、账号变化、设置变化和正常的提示增长；它不会修改请求体，也不会重放缓存未命中。

### 5. 模型管理（国际 / 国内）

点击面板工具栏的 **⚙ 国际模型** 或 **⚙ 国内模型**，可编辑该 realm 的模型目录、前缀、自定义模型和可选回退。列表是上游目录（`/v2/enterprises/personal/models` ∪ `/v3/config`，使用 WorkBuddy 客户端 UA 获取）、插件静态目录与自定义条目的并集。免费（x0.00）模型固定在独立分组，其余按厂商分组、由便宜到贵排列。

- **默认全选**，取消勾选即隐藏（`disabled_models`）。
- **保存**作用于该 realm 的所有账号——按账号的覆盖层已退役，迁移时会移除既有的 `authOverride` 条目。
- **保存/重置**会报告 CPA 是否在不重启的情况下重新加载了 `/v1/models`，并在重新获取期间保留未保存的草稿；关闭弹窗会丢弃草稿。
- **测试**会通过所选账号发出一次真实的（付费）对话请求，显示实际服务的模型、Token、积分和上游错误码——以事实为准，而非目录声明。重新获取只用目录 GET，不消耗对话积分。
- **回退**默认关闭，需要主动开启：缺失模型回退，以及硬错误重试回退（账号切换后仍 403/5xx，带 `X-WB-Fallback-From` 标记，因为响应可能来自不同模型）。

取数账号下拉框显示的昵称与账号卡片一致，请求仍使用稳定的账号 ID。宿主 **Auth Files** 卡片属于 CPAMC 而非本插件，参见 [CPAMC 源码补丁](patches/README.md)。

面板的更新控件显示当前运行版本与最新发行版本；被跳过的版本会与模型管理状态一并保存。

### 6. Realm 识别与前缀

插件同时接受两类签发域（`workbuddy.ai` **和** `codebuddy.ai`），因此国际凭据会路由到正确的计费与对话主机，而不会被送到国内网关（后者返回非 JSON 的 401，表现为 `parse failed: invalid character '<'`）。

两个 realm 各有前缀，客户端可借此显式指定 realm：

| Realm | 默认前缀 | 状态 |
| --- | --- | --- |
| 国内 | `cn/` | **默认开启**——`cn/glm-5.3` 会选中一个国内账号 |
| 国际 | `cb/` | **默认关闭**——开启后可用 `cb/gpt-6.1-sol` |

前缀在请求到达 CodeBuddy 之前被移除。带前缀的请求只在该 realm 内重试，即使 CPA 最初把请求路由到了别的账号。裸模型 ID 保持 CPA 原有路由行为——但如果两个 realm 都不加前缀，同时存在于两边的 ID（`glm-5.3`、`glm-5.2` 等）会产生歧义，面板会给出警告。前缀可在模型管理中修改，留空则使用默认值。

### 7. 账号面板

账号列表默认是**表格**，可切换到卡片。两种视图渲染同一份筛选后的列表，因此搜索、国内/国际/已耗尽筛选、排序和分页对两者同样有效。

- 列：账号、realm 与套餐、实时并发、积分与进度条、每日报告状态、状态、今日请求数、操作。
- **展开行**可看到该账号的每一个积分包，按最早到期排序，并带每个包的实时倒计时。
- **每行操作是"报告"（国际）或"签到"（国内）。** 刷新移到了批量操作栏：勾选若干行后点**刷新数据**即刷新这些行。
- 缺失的数据显示 `—`。面板不会声称一个尚未读取到的状态。

### 8. OAuth 登录区域（国际 / 国内）

OAuth 按钮会在工具栏**登录区域**下拉框所选的那个区域上签发登录 state（默认**国际**）：

- **国际（codebuddy.ai）** —— 登录地址是 `https://www.codebuddy.ai/login?platform=ide&state=…`，页面停留在 `.ai`，国际账号可以走完整个流程。
- **国内（copilot.tencent.com）** —— 与原来一致的国内签发方（`platform=CLI`）。登录国内账号前请先切换。

签发方不是装饰性的：登录 state 是绑定区域的。在 `copilot.tencent.com` 上签发的 state 会打开一个跳转到 `codebuddy.cn` 的登录页，国际账号在那里无法完成登录（issue #5）；反过来，`.cn` 页面也会拒绝在 `.ai` 上签发的 state（HTTP 400）。这个选择就是一条普通的插件配置：`plugins.configs.workbuddy` 下的 `oauth_region: intl|cn`、环境变量 `WB_OAUTH_REGION`，或面板上的下拉框（与签到 / 每日奖励开关一样是运行时生效，重启后以配置文件为准）。

### 9. 继承自原插件

多账号 OAuth 登录、动态模型列表、对话执行器、国际账号一次性 250 积分专家体验包（14 天、每账号限一次）、积分生命周期（国内耗尽禁用 / 国际耗尽删除）、每日 22:00 的 Token 保活，以及带国内/国际筛选的积分面板。

---

## 自行验证

用任意国际账号，上报前后对比：

```bash
# 1. 今天是否已点亮？（score > 0 表示已点亮）
curl -s -X GET 'https://www.codebuddy.ai/activity/growth/heatmap' \
  -H "Authorization: Bearer $TOKEN" -H "X-User-Id: $UID" -H "X-Domain: codebuddy.ai"

# 2. 上报一个活跃事件
curl -s -X POST 'https://www.codebuddy.ai/v2/report' \
  -H "Authorization: Bearer $TOKEN" -H "X-User-Id: $UID" -H "X-Domain: codebuddy.ai" \
  -H 'Content-Type: application/json' \
  -d '[{"eventCode":"chat_request_send","timestamp":1700000000000,"mode":"craft",
        "conversationId":"wb-<uuid>","requestId":"<uuid>","inputLength":12,
        "requestModelId":"deepseek-v4-flash","requestModelName":"DeepSeek V4 Flash",
        "presentAt":1700000000000,"rootRequestId":"<uuid>","parentConversationId":"wb-<uuid>",
        "agentName":"default","agentType":"conversation","userId":"'"$UID"'"}]'

# 3. 今天的热力图格子此时 score 为 2。
# 4. 次日确认奖励包是否到账：
curl -s -X POST 'https://www.codebuddy.ai/v2/billing/meter/get-user-resource' \
  -H "Authorization: Bearer $TOKEN" -H "X-User-Id: $UID" -H "X-Domain: codebuddy.ai" \
  -H 'Content-Type: application/json' -d '{}'
#    → 查找 PackageCode 以 TCACA_code_007 开头的 "Bonus Pack"
```

奖励在次日中国时间凌晨到账。热力图对一次成功的上报有几秒钟延迟，请重新读取而不是直接判定失败。

插件会为每个账号输出一行日志（定时和手动执行都有），因此可以直接从宿主日志确认上报，而不必从积分反推：

```
[workbuddy] daily-report: auth=… day=… lit=… score=… status=reported|already-lit|failed|skipped
[workbuddy] checkin: auth=… status=checked|already|unsupported|inactive|failed
```

---

## 安装

### 通过插件源安装（推荐）

管理中心 → **配置面板 → 高级 → 第三方插件源**，填入以下任一地址：

```
https://github.com/zidanefaqih/codebuddy-intl-cpa/releases/latest/download/registry.json
https://raw.githubusercontent.com/zidanefaqih/codebuddy-intl-cpa/main/registry.json
```

随发行包附带的副本由 CI 打标，始终对应最新发布的版本；`main` 分支的副本也会在发布时同步更新。

或在宿主配置中启用（插件系统默认关闭）：

```yaml
plugins:
  enabled: true
  store-sources:
    - https://raw.githubusercontent.com/zidanefaqih/codebuddy-intl-cpa/main/registry.json
```

> ⚠️ **自行编译的情况：** 通过商店安装会把版本记录在 `store:` 下，宿主随后会移除该插件的其他库文件，包括手工编译的 `.so`。请另开一个宿主或容器来测试商店安装路径。

### 手动安装

```bash
# linux/amd64
unzip workbuddy_<version>_linux_amd64.zip     # 压缩包根目录即 workbuddy.so
cp workbuddy.so /path/to/cliproxyapi/plugins/
```

也支持按平台分目录：`plugins/linux/amd64/workbuddy.so`。

---

## 兼容性

| | |
| --- | --- |
| 最低宿主版本 | CLIProxyAPI 7.2.30（编译所用 SDK）；已在 7.2.145 及当前宿主版本上验证 |
| 构建目标 | linux/amd64、linux/arm64、darwin/amd64、darwin/arm64、windows/amd64、windows/arm64 |
| 插件标识 | `workbuddy` |
| Go | 1.26 |

### 环境变量

| 变量 | 作用 |
| --- | --- |
| `WORKBUDDY_DATA_DIR` | `workbuddy-state.json`、`session-affinity.json` 和用量日志所在目录，默认为认证目录下的 `.codebuddy_data`。 |
| `WB_CACHE_TRACE` | 设为 `1`/`true`/`yes`/`on` 时记录不含内容的请求指纹，用于排查提示缓存。**默认关闭**，见[缓存追踪](workbuddy/docs/cache-tracing.md)。 |
| `WB_MANAGEMENT_KEY` | 配置未提供管理密钥时的回退值。 |
| `WB_SELF_URL` | 保存后触发宿主重新加载模型注册表所用的基础地址。 |
| `USAGE_REPORT_URL`、`USAGE_REPORT_KEY` | 可选的 CPAMP 用量导入地址与密钥。 |

### 已知限制：Intel Mac

CPA 插件是加载**进 Go 宿主进程**的 Go 共享库，而上游 Go 在 **darwin/amd64** 上不支持这种组合。在该平台上，运行时把当前 goroutine 放在一个固定的 TLS 槽位（`0x30(GS)`），宿主与插件会共用它；宿主可能读到插件的运行时状态并以 `fatal error: bad sweepgen in refill` 中止。参见 [golang/go#40131](https://github.com/golang/go/issues/40131) 与 [#38692](https://github.com/golang/go/issues/38692)，两者目前仍处于开启状态。

插件已让加载期不产生任何 goroutine 和定时器以避免干扰，但底层冲突位于 Go 运行时，无法从插件侧消除。

- **Apple Silicon（darwin/arm64）、Linux、Windows 不受影响**——这些平台的运行时使用各自独立的 TLS。
- 在 **Intel Mac** 上，建议把 CPA 跑在容器里（linux/amd64），而不要原生加载插件。

---

## 安全与致谢

CPA 插件作为原生共享库在宿主进程内运行，拥有宿主可访问的文件、网络和凭据权限。请使用可信源码，并核验发行包的 SHA-256 校验值。

发行包附带校验和（`checksums.txt`，SHA-256），由 GitHub Actions 从打标的提交构建，因此发布的二进制与仓库源码一致。

本插件除被授予的账号凭据外不保存任何机密，也不会把 Token 内容打印到日志或面板。

原始执行器和账号提供方实现来自 `Sliverkiss/cpa-plugin`（MIT，已在 GitHub 上删除），其基础来自 `lovingfish` 的 `workbuddy`。感谢两位作者——本项目之所以存在，是因为那些工作曾被公开。

其余部分均为本仓库自身的工作：429 与积分耗尽切换、准入控制与会话粘性、逐请求用量/积分记录、模型管理、国际版 realm 处理，以及国际账号每日奖励引擎。

许可证：MIT，见 [LICENSE](LICENSE)。更多开发资料见[开发指南](workbuddy/docs/development.md)。
