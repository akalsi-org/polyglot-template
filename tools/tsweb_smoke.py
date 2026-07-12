#!/usr/bin/env python3
"""Validate the production React bundle without serving source-tree files."""

from __future__ import annotations

import argparse
import re
from pathlib import Path


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--max-js-bytes", type=int, default=250_000)
    args = parser.parse_args()

    root = args.root.resolve()
    index = root / "index.html"
    html = index.read_text(encoding="utf-8")
    if 'id="root"' not in html:
        raise SystemExit("tsweb smoke: production index is missing #root")

    references = re.findall(r'(?:src|href)="(/assets/[^"]+)"', html)
    if not references:
        raise SystemExit("tsweb smoke: production index has no bundled assets")
    for reference in references:
        if not (root / reference.removeprefix("/")).is_file():
            raise SystemExit(f"tsweb smoke: missing referenced asset {reference}")

    scripts = sorted(root.glob("assets/*.js"))
    js_bytes = sum(path.stat().st_size for path in scripts)
    if not scripts or js_bytes > args.max_js_bytes:
        raise SystemExit(
            f"tsweb smoke: JavaScript budget exceeded ({js_bytes} > {args.max_js_bytes})"
        )
    print(f"tsweb smoke: ok ({js_bytes} JavaScript bytes)")


if __name__ == "__main__":
    main()
