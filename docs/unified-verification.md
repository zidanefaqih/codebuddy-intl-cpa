# Unified build verification — 2026-10-18

Build: `0.9.7+unified-trae.0.1.6`, Windows amd64, CPA 8.0.23.

The deployed binary below was built from integration commit `f58889e`.
The upstream submission additionally restores the upstream repository metadata,
limits the local-build update override to `+unified-trae.*`, and makes the
installation example portable. Those submission changes were checked with the
Go regression suite and vet; the installed binary was left in place.

DLL SHA-256:
`61693A2A050052AE897ED6B2CD0DBFBDA7DDABC9D5BAD3239505868EB4EBE879`

Completed checks:

- `go test -race -count=1 ./...` — passed, including embedded Trae tests,
  migration tests, channel boundaries, per-account model filtering, settings
  preservation through token rotation/re-import, and separate credit journals.
- `go vet ./...` — passed.
- Existing CodeBuddy panel tests — **64 passed**.
- Unified panel Playwright checks — passed for bilingual tabs, embedded
  CodeBuddy panel, model drafts/save, credential import, pending operations,
  usage filters, authentication rejection, desktop/mobile and light/dark UI.
- Native C host fixture — loaded the real DLL, parsed both providers,
  discovered prefixed Trae models, completed and streamed both protocols,
  rejected crossed model/credential requests. HTTP used synthetic fixtures.
- Isolated real CPA — loaded one DLL; registration, independent Trae config,
  overview, both panel resources and management authentication passed.

Installed into the user's existing CPA installation with automatic backups.
The original Trae DLL was moved outside the loader directory; exactly one DLL
remains. Two Trae credential files were migrated; their original top-level
settings were compared against backups and retained.

Installed CPA checks:

- Unified panel: HTTP 200.
- Authenticated `/v1/models`: HTTP 200, including **45 CodeBuddy models** and
  **12 `trae/` models**. Other built-in provider models remain available.
- CPA process running after installation.

Live paid chat was not part of this verification; chat and streaming were
exercised through the real DLL with mocked host HTTP callbacks.
