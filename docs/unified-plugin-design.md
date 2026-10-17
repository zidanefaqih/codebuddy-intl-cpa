# Unified CodeBuddy and Trae plugin

Approved direction: one native library and one CPA management entry, with
CodeBuddy CN/Global and Trae CN implemented as isolated channels.

## Compatibility

- Keep the CPA provider/plugin ID `workbuddy`, preserving CodeBuddy credentials,
  settings, model prefixes and existing routes.
- Embed the Trae 0.1.6 engine under `workbuddy/internal/trae`, retaining its MIT
  license and attribution. Use the CPA 8.0.23 SDK throughout.
- Store managed Trae credentials with `type: workbuddy` and `channel: trae`,
  retaining their `trae-<uid>.json` filename, original token/account fields,
  disabled state and routing settings. Legacy files are explicitly migrated
  with backups; import accepts the old Trae JSON as well.
- Emit Trae model IDs under `trae/`; keep CodeBuddy's existing CN/Global routing.
  Never send a Trae credential to CodeBuddy or vice versa, including on failover.
- Keep token refresh, sign-in/check-in, cooldowns, caches and account locks
  channel-local. Boundary adapters translate only the host provider identity.

## Management

- A single bilingual landing page provides overview, CodeBuddy and Trae sections.
- Retain the complete CodeBuddy model/account management UI; the Trae section
  provides login/import, balance/check-in, enable/disable, model list/visibility
  and request history through the same native library.
- Shared overview and usage show each channel explicitly. Balances are reported
  separately because the providers' credits are not interchangeable.
- No secrets in overview responses, logs or browser persistence. Management
  requests retain the existing CPA authentication boundary.

## Delivery sequence

1. Isolated integration branch based on official main plus the compatibility fix.
2. Embed and adapt the Trae engine, registration, auth, model and executor routes.
3. Add unified management, migration tooling and attribution.
4. Verify channel isolation, credential preservation, existing provider tests,
   UI behavior, Windows c-shared build and CPA loading/request compatibility.
5. Keep the integration separate from the upstream SSE/Responses PR.
