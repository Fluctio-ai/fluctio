#!/usr/bin/env python3
"""Extract a unified model metadata table from docs/models.json.

Input : docs/models.json             (array form: {"models": [{id, contextWindow, maxOutputTokens, ...}]})
Output: internal/config/model_meta.json  (fluctio embed source)

Each entry carries ``contextWindow`` (input token limit) and
``maxOutputTokens`` (output token limit). We project those two fields into a
flat ``{model-id: {contextWindow, maxTokens}}`` map. Entries without an id or
a contextWindow are skipped (contextWindow is the load-bearing field; a
missing maxOutputTokens is recorded as 0).

Run:  python scripts/extract_model_meta.py
Re-run whenever docs/models.json is refreshed.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path


def _as_int(val) -> int | None:
    """Coerce val to int when it is a real integer (bool rejected)."""
    if isinstance(val, bool):
        return None
    if isinstance(val, int):
        return val
    if isinstance(val, float) and val.is_integer():
        return int(val)
    return None


def extract(models: list) -> dict[str, dict[str, int]]:
    out: dict[str, dict[str, int]] = {}
    for m in models:
        if not isinstance(m, dict):
            continue
        mid = m.get("id")
        cw = _as_int(m.get("contextWindow"))
        if not mid or cw is None or cw <= 0:
            continue  # need at least an id + a usable contextWindow
        mt = _as_int(m.get("maxOutputTokens"))
        out[mid] = {
            "contextWindow": cw,
            "maxTokens": mt if (mt is not None and mt > 0) else 0,
        }
    return out


def main() -> int:
    repo = Path(__file__).resolve().parent.parent
    src = repo / "docs" / "models.json"
    dst = repo / "internal" / "config" / "model_meta.json"

    with src.open("r", encoding="utf-8") as fh:
        data = json.load(fh)

    models = data.get("models", []) if isinstance(data, dict) else []
    out = extract(models)

    with dst.open("w", encoding="utf-8") as fh:
        json.dump(out, fh, ensure_ascii=False, indent=2, sort_keys=True)
        fh.write("\n")

    print(f"wrote {len(out)} models -> {dst.relative_to(repo)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
