#!/usr/bin/env python3
"""Minimal CPA pluginstore registry.json validator (schema v1 github-release)."""
from __future__ import annotations

import json
import re
import sys
from pathlib import Path

ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
VER_RE = re.compile(r"^[0-9][0-9A-Za-z.+-]*$")


def main() -> int:
    path = Path(sys.argv[1] if len(sys.argv) > 1 else "registry.json")
    data = json.loads(path.read_text())
    sv = data.get("schema_version")
    if sv not in (1, 2):
        raise SystemExit(f"unsupported schema_version {sv}")
    plugins = data.get("plugins") or []
    if not plugins:
        raise SystemExit("plugins empty")
    seen = set()
    for i, p in enumerate(plugins):
        for field in ("id", "name", "description", "author"):
            if not str(p.get(field, "")).strip():
                raise SystemExit(f"plugins[{i}]: missing {field}")
        pid = p["id"].strip()
        if not ID_RE.match(pid):
            raise SystemExit(f"plugins[{i}]: invalid id {pid!r}")
        if pid in seen:
            raise SystemExit(f"duplicate id {pid}")
        seen.add(pid)
        ver = str(p.get("version") or "").strip()
        if ver and not VER_RE.match(ver):
            raise SystemExit(f"plugins[{i}]: invalid version {ver!r}")
        install = p.get("install") or {}
        itype = (install.get("type") or "github-release").strip()
        if itype == "github-release":
            repo = str(p.get("repository") or "").strip()
            if "github.com" not in repo:
                raise SystemExit(f"plugins[{i}]: repository must be github for github-release")
        elif itype == "direct":
            if sv != 2:
                raise SystemExit("direct install requires schema_version 2")
            if not ver:
                raise SystemExit(f"plugins[{i}]: direct install needs version")
        else:
            raise SystemExit(f"plugins[{i}]: bad install type {itype}")
    print(f"OK {path}: {len(plugins)} plugin(s), schema_version={sv}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
