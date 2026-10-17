# CodeBuddy Intl CPA

> **Unified CodeBuddy + Trae build:** this branch embeds Trae CN SOLO into the
> same Windows DLL and bilingual management entry. See [installation and
> compatibility notes](docs/unified-plugin.md). Internal provider ID: `workbuddy`.

[简体中文](README.zh-CN.md) | English

A **CLIProxyAPI (CPA)** provider plugin for **Tencent CodeBuddy**, built for **Global (Intl)
accounts** — including the daily credit reward that other CPA plugins skip.

The account panel supports **中文 / English** via its top-right language selector.
Changes apply immediately and are remembered in the current browser. The initial
language follows the browser's language.

> **On the name.** The project is `codebuddy-intl-cpa`. The plugin it ships keeps the internal
> id `workbuddy` — the file `workbuddy.so`, the auth type `workbuddy`, the credential files
> `workbuddy-<uid>.json`, the config key `plugins.configs.workbuddy`, and the release asset
> names. That id is a host-side contract shared with every existing install and with the
> credential files already on disk, so renaming it would orphan them. Everything a human reads
> — this README, the store listing, the plugin metadata, the panel, the release title — says
> **CodeBuddy Intl CPA**.

> **Status:** runs in production. 31 Go files (~13.6k lines) plus 39 test files, and a
> 3,117-line management panel embedded with `go:embed`.

---

## Why this one

The CPA plugins for this provider that exist today are **CN-first**: they gate the growth centre
to the domestic realm, on the assumption that the endpoint family simply does not exist
overseas. That assumption is wrong, and it leaves Global accounts with no daily income at
all — just the flat 100 credits/month free plan.

This plugin does the opposite:

| | Other CPA plugins | This plugin |
| --- | --- | --- |
| Growth centre (`/v2/report`, heatmap, streak) | CN realm only | **CN + Global** |
| Daily credit reward on Global accounts | ✗ | ✅ **30 credits/day** |
| 429 / `code 6004` / credit-exhaustion failover | falls through to the host | **in-plugin, per-account** |
| Per-request credit / token log | ✗ | ✅ **JSONL + panel** |
| Per-realm model catalog and prefix | hardcoded | ✅ **Model Manager** |

---

## Features

### 1. Daily credit reward for Global accounts (headline)

Global accounts can earn **30 credits/day per account** through the growth centre, with
no desktop app required.

How it works: the growth centre counts an *activity day* when the client reports one
event to `POST /v2/report` (`eventCode: chat_request_send`). A lit day is visible as a
non-zero `score` in `GET /activity/growth/heatmap`, and the next day the account receives
a **Bonus Pack** of 30 credits (`TCACA_code_007_nzdH5h4Nl0`, 平台奖励积分, valid 30 days).

Verified **2026-09-28 → 2026-09-29** on 7 accounts: all six Global accounts that were lit
by the report alone received their 30-credit pack the following morning, plus one account
lit by a plain client login. See [How to verify](#how-to-verify-it-yourself).

The plugin drives this on a schedule and is **idempotent per account per day** — it reads
the heatmap first and only reports days that are not lit yet, so extra runs are free.

| | |
| --- | --- |
| Schedule | `08:00 / 12:00 / 16:00 / 20:00` in the plugin's local time, on the shared plugin scheduler tick |
| Config | `daily_bonus: true` (default on) |
| Panel | **Daily bonus (Global)** toggle, plus a per-row **Report** button |
| Manual run | `POST /v0/management/plugins/workbuddy/dailybonus` (`{"auth_index":"…"}` for one account) |
| Status | `GET /v0/management/plugins/workbuddy/dailybonus/status` — per account: day, score, lit dates |

Behaviour worth knowing:

- **"Today" comes from the API's own heatmap** (its last cell), not from local time — so
there is no timezone arithmetic and China-time day boundaries need no special handling.
- **Four ticks a day.** Because the runner skips days that are already lit, the extra
ticks are free and a tick that fails (e.g. the network is not up yet right after boot)
is covered by the next one.
- **The report POST is never retried.** The event is day-idempotent upstream, but a blind
resend would double-count the day (`score` 2 → 4). A failed send simply waits for the
next tick.
- **Global accounts only.** CN accounts keep using the check-in path, and CN growth lives
on a different host (`copilot.tencent.com`).
- **The schedule follows the CPA process timezone**, i.e. the container's `TZ`, not the
browser's. A container left on `Asia/Shanghai` runs these four ticks at 07:00 / 11:00 /
15:00 / 19:00 WIB. The reward itself is credited on China time regardless, because "today"
is read from the API rather than computed locally.
- **Report all is paced, not a burst.** The bulk button walks the accounts one at a time
with a 1.5s gap, matching the spacing of the host's own report script. A parallel burst
is what produced upstream 429 storms.
- **The per-row button is honest.** It only offers **Report** when the day is known to be
unlit; once lit it reads **Lit** and is disabled. Accounts with no data yet show `—`
rather than a claim.

**Bonus:** consecutive days also unlock streak tiers (`14d` → +50 credits,
`28d` → +150 credits) on top of the daily amount.

### 2. CN daily check-in

CN accounts keep their own reward path: a daily check-in worth **100 credits/day** plus
500/month. The plugin runs it at **09:00 / 21:00** in the CPA process timezone when
`checkin_auto` is on, and the panel offers a per-row **Check in** button and a **Check all**
bulk action. Check-ins are serialized per account so two browser tabs cannot double-submit.

### 3. In-plugin account failover

CPA's built-in cooldown and retry only apply to in-host executors. A `c-shared` plugin
performs its own upstream requests, so a `429` / `code 6004` never reaches the host
selector and no failover happens — the request just fails.

This plugin keeps a per-account cooldown map and retries the **same request** on another
enabled account before returning an error (`failover.go`, `retry.go`).

Explicit credit exhaustion (HTTP 402, code 14018, `Credits exhausted`) is handled before
any output chunk. The plugin retries in the same realm and quarantines the rejected account
until the reported reset, or for 30 minutes. Quarantine is process-local and clears on
restart. It works with `lifecycle_auto:false` and does not disable or delete auth files.
Partial output, empty HTTP 200 streams, and transport errors stop the request without
replay. Failover can incur a prompt-cache miss on the replacement account; cache sharing
varies by model and backend.

**Admission control.** Chat execution is limited to **two active requests per account**
across models and duplicate host identities (UID + realm). If both slots are full the
executor immediately tries another enabled account in the same realm; if every account is
busy it fails fast without sending a paid upstream request. There is **no local waiting
queue**. Quarantine is rechecked after each slot is reserved, so a concurrent request cannot
keep sending to an account that was just rejected with 429. The panel shows the live
`current/limit` badge (e.g. `0/2`) from the admission gate itself.

**Soft session affinity.** A conversation keeps using its last executing account while a
slot is free, moves to a free peer when that account is busy, and keeps the replacement so
the prompt cache is not bounced back. The affinity key is a digest of client credential +
stable conversation identity + model + realm. Bindings are capped, expire after an hour,
and persist to `session-affinity.json` so a restart does not move a conversation.

Routing is always per session: **there is no panel-selected active account**, because one
global choice leaked across conversations and forced a prompt-cache re-write on models that
do not share cache across accounts.

Soft 429/`code 6004` quarantine is **account + upstream model**; credit exhaustion remains
account-wide. When every account for a model is cooling, a request may probe one
soft-limited account after 30 seconds, one probe at a time per account. A fresh rejection
renews the backoff. The plugin does not probe exhausted or disabled accounts, cross realms,
or replay partial/empty/transport-failed responses. Upstream can still reject for RPM,
token-volume or shared quotas — admission cannot guarantee upstream capacity.

Separate CPA processes have separate limits, so prod and dev must not use the same account
concurrently. Switching accounts may lose prompt cache on `gpt-6-sol` and `gpt-6.1-sol`;
the account limit is not a cache-sharing guarantee.

### 4. Per-request usage and credit log

Every chat request that flows through the plugin is recorded — account, model, input /
output / cached tokens, and the upstream `credit` value taken from the terminal SSE chunk.
Stored append-only as JSONL next to the auth store, with rotation at ~50 MB
(`request-usage-<YYYY-MM>.jsonl`, older data retained). Powers the **Usage CodeBuddy**
panel section. No credentials are ever written.

This is also what made the cache economics visible: a 24k-token request with a full cache
hit bills ~0.05 credit instead of ~0.62.

For diagnosing transient prompt-cache misses the plugin can additionally record
**content-free request hashes** and the actual outgoing conversation id. That is
**off by default** — set `WB_CACHE_TRACE=1` in the plugin's environment to collect
it, because the fingerprints otherwise dominate the accounting log (measured at
99.4% of each line) and slow the usage view down. See
[cache tracing](workbuddy/docs/cache-tracing.md) for the fields, the privacy limits, and the
comparison script. Per-option digests identify *which* known setting changed without
recording its value. `scripts/study-cache-misses.py` produces a private, metadata-only report
across active and rotated logs, separating prefix loss, account changes, changed settings and
normal prompt growth. It never changes a request body or replays a cache miss.

### 5. Model Manager (Global / CN)

Open **⚙ Global Models** or **⚙ CN Models** in the panel toolbar to edit the realm catalog,
prefix, custom models, and optional fallbacks. The list is the union of the upstream catalog
(`/v2/enterprises/personal/models` ∪ `/v3/config`, fetched with the WorkBuddy client UA), the
plugin's static catalog, and your custom entries. Free (x0.00) models are pinned to a
dedicated section; the rest are grouped by vendor, cheapest first.

- **Fetch-all by default**; uncheck to hide a model (`disabled_models`).
- **Save** applies to every account in that realm — per-account overrides are retired, and
  existing `authOverride` entries are removed during migration.
- **Save/Reset** reports whether CPA reloaded `/v1/models` without a restart, and preserves
  unsaved drafts while a re-fetch is in flight. Closing the popup discards them.
- **Test** sends a real (paid) chat request through the selected account and shows the served
  model, tokens, credit and the upstream error code — ground truth instead of catalog claims.
  Re-fetch uses catalog GETs only and costs no chat credits.
- **Fallbacks** are opt-in and off by default: a missing-model fallback, and a
  retry-on-hard-error fallback (403/5xx after account failover, marked with
  `X-WB-Fallback-From` because the response may come from a different model).

Fetch-account dropdowns display the same nickname as the account cards while requests keep
their stable account IDs. For the host **Auth Files** cards, which belong to CPAMC rather
than this plugin, see the [CPAMC source patch](patches/README.md).

The panel's update widget shows the running and latest release versions; skipped releases
persist beside the Model Manager state.

### 6. Realm handling and per-realm prefixes

Realm detection accepts both issuers (`workbuddy.ai` **and** `codebuddy.ai`), so Intl
credentials route to the correct billing and chat hosts instead of being sent to the CN
gateway (which answers a non-JSON 401 and surfaces as `parse failed: invalid character '<'`).

Each realm has its own prefix, so a client can pick the realm explicitly:

| Realm | Default prefix | State |
| --- | --- | --- |
| CN | `cn/` | **on** by default — `cn/glm-5.3` selects a CN account |
| Global | `cb/` | **off** by default — enable it to route `cb/gpt-6.1-sol` |

The prefix is stripped before the request reaches CodeBuddy. Requests carrying a prefix retry
only within that realm, even if CPA initially routed them elsewhere. Bare model IDs keep the
normal CPA routing behavior — but if both realms are exposed without prefixes, IDs that exist
in both (`glm-5.3`, `glm-5.2`, …) become ambiguous and the panel warns about it. Prefixes are
editable in Model Manager; leaving the value blank uses the default.

### 7. Account panel

The account list defaults to a **table** with a card toggle. Both views render from the same
filtered list, so search, the CN / Global / Exhausted filters, sorting and paging apply to
either.

- Columns: account, realm and plan, live concurrency, credits with a progress bar, daily
  report state, status, today's requests, and actions.
- **Expand a row** to see every credit package it holds, sorted expiring-first with a live
  per-package countdown.
- **Row actions are Report** (Global) or **Check in** (CN). Refresh moved to the bulk bar:
  checking rows and pressing **Refresh data** refreshes those rows.
- Missing data renders as `—`. The panel never claims a state it has not read.

### 8. Inherited from the original plugin

Multi-account OAuth login, dynamic model list, chat executor, Global one-time 250-credit
expert trial (14 days, once per account), credit lifecycle (disable CN / delete Global when
exhausted), token keepalive at 22:00, and the credits panel with CN/Global filters.

---

## How to verify it yourself

With any Global (Intl) account, before and after:

```bash
# 1. Is today already lit?  (score > 0 means yes)
curl -s -X GET 'https://www.codebuddy.ai/activity/growth/heatmap' \
  -H "Authorization: Bearer $TOKEN" -H "X-User-Id: $UID" -H "X-Domain: codebuddy.ai"

# 2. Report one activity event
curl -s -X POST 'https://www.codebuddy.ai/v2/report' \
  -H "Authorization: Bearer $TOKEN" -H "X-User-Id: $UID" -H "X-Domain: codebuddy.ai" \
  -H 'Content-Type: application/json' \
  -d '[{"eventCode":"chat_request_send","timestamp":1700000000000,"mode":"craft",
        "conversationId":"wb-<uuid>","requestId":"<uuid>","inputLength":12,
        "requestModelId":"deepseek-v4-flash","requestModelName":"DeepSeek V4 Flash",
        "presentAt":1700000000000,"rootRequestId":"<uuid>","parentConversationId":"wb-<uuid>",
        "agentName":"default","agentType":"conversation","userId":"'"$UID"'"}]'

# 3. The heatmap cell for today now has score 2.
# 4. Next day, check the pack arrived:
curl -s -X POST 'https://www.codebuddy.ai/v2/billing/meter/get-user-resource' \
  -H "Authorization: Bearer $TOKEN" -H "X-User-Id: $UID" -H "X-Domain: codebuddy.ai" \
  -H 'Content-Type: application/json' -d '{}'
#    → look for a "Bonus Pack" whose PackageCode starts with TCACA_code_007
```

The reward lands the next day, in the early hours of the morning China time. The heatmap
lags a successful report by a few seconds; re-read it rather than assuming failure.

The plugin logs one line per account for both scheduled and manual runs, so a report can be
confirmed from the host log instead of inferred from credits:

```
[workbuddy] daily-report: auth=… day=… lit=… score=… status=reported|already-lit|failed|skipped
[workbuddy] checkin: auth=… status=checked|already|unsupported|inactive|failed
```

---

## Install

### From a plugin source (recommended)

Management Center → **Config Panel → Advanced → Third-party Plugin Sources**, add one of:

```
https://github.com/zidanefaqih/codebuddy-intl-cpa/releases/latest/download/registry.json
https://raw.githubusercontent.com/zidanefaqih/codebuddy-intl-cpa/main/registry.json
```

The release-attached copy is stamped by CI and always matches the newest published release.
The `main` branch copy is updated on release as well.

Or in the host config (the plugin system is off by default):

```yaml
plugins:
  enabled: true
  store-sources:
    - https://raw.githubusercontent.com/zidanefaqih/codebuddy-intl-cpa/main/registry.json
```

> ⚠️ **If you built the plugin yourself:** a store install records the version under
> `store:` and the host then removes the plugin's other library files, including a
> hand-built `.so`. Use a separate host/container to try the store path.

### Manual

```bash
# linux/amd64
unzip workbuddy_<version>_linux_amd64.zip     # contains workbuddy.so at the zip root
cp workbuddy.so /path/to/cliproxyapi/plugins/
```

Platform layout also works: `plugins/linux/amd64/workbuddy.so`.

---

## Compatibility

| | |
| --- | --- |
| Minimum host | CLIProxyAPI 7.2.30 (the SDK it builds against); verified against 7.2.145 and current host releases |
| Built for | linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64, windows/arm64 |
| Plugin id | `workbuddy` |
| Go | 1.26 |

### Environment

| Variable | Effect |
| --- | --- |
| `WORKBUDDY_DATA_DIR` | Where `workbuddy-state.json`, `session-affinity.json` and the usage logs live. Defaults to `.codebuddy_data` under the auth dir. |
| `WB_CACHE_TRACE` | `1`/`true`/`yes`/`on` records content-free request fingerprints for prompt-cache investigations. **Off by default** — see [cache tracing](workbuddy/docs/cache-tracing.md). |
| `WB_MANAGEMENT_KEY` | Fallback management key when the config does not set one. |
| `WB_SELF_URL` | Base URL the plugin uses to trigger a host model-registry reload after Save. |
| `USAGE_REPORT_URL`, `USAGE_REPORT_KEY` | Optional CPAMP usage import target and key. |

### Known limitation: Intel Macs

A CPA plugin is a Go shared library loaded **into the Go host process**, and upstream Go
does not support that combination on **darwin/amd64**. On that one target the runtime keeps
the current goroutine in a single hardcoded TLS slot (`0x30(GS)`), which the host and the
plugin then share; the host can end up reading the plugin's runtime state and abort with
`fatal error: bad sweepgen in refill`. See [golang/go#40131](https://github.com/golang/go/issues/40131)
and [#38692](https://github.com/golang/go/issues/38692), both still open.

The plugin keeps load time free of goroutines and timers to stay out of the way, but the
underlying conflict is in the Go runtime and cannot be removed from the plugin side.

- **Apple Silicon (darwin/arm64), Linux and Windows are unaffected** — the runtime uses a
  proper per-library TLS there.
- On an **Intel Mac**, prefer running CPA in a container (linux/amd64) rather than loading
  the plugin natively.

---

## Security

A CPA plugin is a **native shared library loaded into the host process** and runs with all
of the host's access — filesystem, network, and the credentials you have configured. Only
install plugins whose source you trust, and verify the release checksums.

Releases are checksummed (`checksums.txt`, SHA-256) and built by GitHub Actions from a
tagged commit, so the published binaries correspond to the source in this repository.

This plugin stores no secrets beyond the account credentials it is given, and never prints
token material to logs or the panel.

---

## Credits

The executor and provider base came from **`Sliverkiss/cpa-plugin`** (MIT, since deleted
from GitHub), which itself was based on **`workbuddy` by `lovingfish`**. Thank you to both
— this project exists because that work was published.

Everything else is this repository's own work: the 429 and credit-exhaustion failover, the
admission control and session affinity, the per-request usage/credit log, the Model Manager,
the Intl realm handling, and the Global daily reward engine.

## License

MIT — see [LICENSE](LICENSE).
