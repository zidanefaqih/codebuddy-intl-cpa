# CodeBuddy Intl CPA

A **CLIProxyAPI (CPA)** provider plugin for **Tencent CodeBuddy**, built for **Global (Intl)
accounts** — including the daily credit reward that other CPA plugins skip.

> **On the name.** The project is `codebuddy-intl-cpa`. The plugin it ships keeps the internal
> id `workbuddy` — the file `workbuddy.so`, the auth type `workbuddy`, the credential files
> `workbuddy-<uid>.json`, the config key `plugins.configs.workbuddy`, and the release asset
> names. That id is a host-side contract shared with every existing install and with the
> credential files already on disk, so renaming it would orphan them. Everything a human reads
> — this README, the store listing, the plugin metadata, the panel, the release title — says
> **CodeBuddy Intl CPA**.

> **Status:** runs in production. 46 Go files (~10.9k lines) + a 1,061-line management panel.
> Verified against CLIProxyAPI **7.2.145**.

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
| 429 / `code 6004` failover | falls through to the host | **in-plugin, per-account** |
| Per-request credit / token log | ✗ | ✅ **JSONL + panel** |
| Hidden upstream models | partial | ✅ **8 models** |

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
by the report alone received their 30-credit pack the following morning (02:14–02:21 WIB),
plus one account lit by a plain client login. See [How to verify](#how-to-verify-it-yourself).

The plugin drives this on a schedule and is **idempotent per account per day** — it reads
the heatmap first and only reports days that are not lit yet, so extra runs are free.

**Built into the plugin — no external cron or script needed:**

| | |
| --- | --- |
| Schedule | `08:00 / 12:00 / 16:00 / 20:00` local, on the shared plugin scheduler tick |
| Config | `daily_bonus: true` (default on) |
| Panel | "Daily bonus (Global)" toggle next to the CN check-in toggle |
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

**Bonus:** consecutive days also unlock streak tiers (`14d` → +50 credits,
`28d` → +150 credits) on top of the daily amount.

### 2. In-plugin 429 failover

CPA's built-in cooldown and retry only apply to in-host executors. A `c-shared` plugin
performs its own upstream requests, so a `429` / `code 6004` never reaches the host
selector and no failover happens — the request just fails.

This plugin keeps a per-account cooldown map and retries the **same request** on another
enabled account before returning an error (`failover.go`, `retry.go`).

### 3. Per-request usage and credit log

Every chat request that flows through the plugin is recorded — account, model, input /
output / cached tokens, and the upstream `credit` value taken from the terminal SSE chunk.
Stored append-only as JSONL next to the auth store, with rotation at ~50 MB
(`request-usage-<YYYY-MM>.jsonl`, older data retained). Powers the **Usage CodeBuddy**
panel section. No credentials are ever written.

This is also what made the cache economics visible: a 24k-token request with a full cache
hit bills ~0.05 credit instead of ~0.62.

### 4. Hidden upstream models

CodeBuddy accepts more models than its own model list advertises. This plugin exposes them
statically so they are routable:

```
gpt-5.6-luna    claude-opus-5    gpt-5.6-sol     glm-5.3
gpt-5.6-terra   deepseek-v4.1-flash   glm-5.3-flash   kimi-k3
```

### 5. Global / Intl realm handling

Realm detection accepts both issuers (`workbuddy.ai` **and** `codebuddy.ai`), so Intl
credentials route to the correct billing and chat hosts instead of being sent to the CN
gateway (which answers a non-JSON 401 and surfaces as `parse failed: invalid character '<'`).

### 6. Inherited from the original plugin

Multi-account OAuth login, dynamic model list, chat executor, CN daily check-in, Global
one-time 250-credit expert trial, credit lifecycle (disable CN / delete Global when
exhausted), and the credits panel with CN/Global filters.

---

## How to verify it yourself

With any Global (Intl) account, before and after:

```bash
# 1. Is today already lit?  (score > 0 means yes)
curl -s -X GET 'https://www.workbuddy.ai/activity/growth/heatmap' \
  -H "Authorization: Bearer $TOKEN" -H "X-User-Id: $UID" -H "X-Domain: codebuddy.ai"

# 2. Report one activity event
curl -s -X POST 'https://www.workbuddy.ai/v2/report' \
  -H "Authorization: Bearer $TOKEN" -H "X-User-Id: $UID" -H "X-Domain: codebuddy.ai" \
  -H 'Content-Type: application/json' \
  -d '[{"eventCode":"chat_request_send","timestamp":1700000000000,"mode":"craft",
        "conversationId":"wb-<uuid>","requestId":"<uuid>","inputLength":12,
        "requestModelId":"deepseek-v4-flash","requestModelName":"DeepSeek V4 Flash",
        "presentAt":1700000000000,"rootRequestId":"<uuid>","parentConversationId":"wb-<uuid>",
        "agentName":"default","agentType":"conversation","userId":"'"$UID"'"}]'

# 3. The heatmap cell for today now has score 2.
# 4. Next day, check the pack arrived:
curl -s -X POST 'https://www.workbuddy.ai/v2/billing/meter/get-user-resource' \
  -H "Authorization: Bearer $TOKEN" -H "X-User-Id: $UID" -H "X-Domain: codebuddy.ai" \
  -H 'Content-Type: application/json' -d '{}'
#    → look for a "Bonus Pack" whose PackageCode starts with TCACA_code_007
```

The reward lands the next day between **00:30 and 03:25 China time**. The heatmap lags a
successful report by a few seconds; re-read it rather than assuming failure.

---

## Install

### From a plugin source (recommended)

Management Center → **Config Panel → Advanced → Third-party Plugin Sources**, add:

```
https://raw.githubusercontent.com/zidanefaqih/codebuddy-intl-cpa/main/registry.json
```

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
| Minimum host | CLIProxyAPI 7.2.30 (verified against 7.2.145) |
| Built for | linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64, windows/arm64 |
| Plugin id | `workbuddy` |
| Go | 1.26 |

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

Everything else is this repository's own work: the 429 failover, the per-request
usage/credit log, the hidden-model list, the Intl realm handling, and the Global daily
reward engine.

## License

MIT — see [LICENSE](LICENSE).
