#!/usr/bin/env python3
"""THIRD_PARTY_NOTICES.md's frontend table names exactly what frontend/package.json ships.

The Go table has had a checker since 2026-08-25 (check-third-party-notices.sh); the
frontend table had none, and drifted the same way: it listed `next` 16.2.10 while the
image shipped 16.3.8, kept seven packages package.json no longer has (axios, zod,
date-fns, react-hook-form, @hookform/resolvers, @radix-ui/react-dialog,
@radix-ui/react-icons) and missed react-grid-layout, which a dashboard renders with.

One property, checked against the manifest rather than a lockfile or node_modules, so it
needs no npm install: the table's (package, version spec) rows are exactly
package.json's `dependencies`, and the two summary lines that restate the table -- the
`_Inventory:` count and the License-distribution line -- agree with it. Licenses
themselves are not re-resolved here; the release-time scripts/gen-third-party-notices.sh
does that from the installed tree.

Stdlib only. Exit 0 = agrees, 1 = drift (with the rows to change), 2 = the file's shape
changed so nothing could be compared.
"""

import json
import os
import re
import sys
from collections import Counter

ROOT = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
NOTICES = os.path.join(ROOT, "THIRD_PARTY_NOTICES.md")
PACKAGE_JSON = os.path.join(ROOT, "frontend", "package.json")

SECTION = "## Frontend (npm, production dependencies)"
# | `name` | spec | License |   or   | [name](url) | spec | License |
ROW = re.compile(r"^\|\s*(?:`([^`]+)`|\[([^\]]+)\]\([^)]*\))\s*\|\s*([^|]+?)\s*\|\s*([^|]+?)\s*\|\s*$")


def table_rows(text):
    """-> {package: (spec, spdx)} from the frontend section, or None if it is gone."""
    rows, body, seen = {}, False, False
    for line in text.splitlines():
        if line.startswith(SECTION):
            body = seen = True
            continue
        if line.startswith("## "):
            body = False
        if not body:
            continue
        m = ROW.match(line)
        if m:
            name = m.group(1) or m.group(2)
            # "EPL-2.0 (elected from `EPL-2.0 OR GPL-3.0-or-later`)" counts as EPL-2.0.
            spdx = m.group(4).split(" (")[0].strip()
            rows[name] = (m.group(3), spdx)
    return rows if seen else None


def main():
    text = open(NOTICES, encoding="utf-8").read()
    with open(PACKAGE_JSON, encoding="utf-8") as fh:
        truth = json.load(fh).get("dependencies") or {}
    rows = table_rows(text)
    if rows is None or not rows or not truth:
        print(f"FATAL: parsed {len(rows or {})} frontend rows and {len(truth)} package.json "
              "dependencies -- the section or the manifest changed shape.")
        return 2

    problems = []
    missing = sorted(set(truth) - set(rows))
    extra = sorted(set(rows) - set(truth))
    stale = sorted(n for n in set(truth) & set(rows) if rows[n][0] != truth[n])
    for n in missing:
        problems.append(f"{n} {truth[n]} ships but is not listed. Add: | `{n}` | {truth[n]} | <SPDX> |")
    for n in extra:
        problems.append(f"{n} is listed but is not in package.json dependencies. Delete its row.")
    for n in stale:
        problems.append(f"{n} is listed at {rows[n][0]}; package.json has {truth[n]}.")

    # The summary lines restate the table, and are what a hand edit forgets.
    n = len(rows)
    inv = re.search(r"^_Inventory:.*?(\d+) frontend \(npm\) direct", text, re.M | re.S)
    if not inv:
        problems.append("the `_Inventory:` line no longer states a frontend (npm) count")
    elif int(inv.group(1)) != n:
        problems.append(f"`_Inventory:` says {inv.group(1)} frontend deps; the table has {n}")
    spdx = Counter(s for _, s in rows.values())
    want = "; ".join(f"{k} x {v}" for k, v in sorted(spdx.items(), key=lambda kv: (-kv[1], kv[0])))
    dist = re.search(r"^- \*\*Frontend\*\* \((\d+) direct prod deps\): (.+?); plus ", text, re.M)
    if not dist:
        problems.append("the License-distribution line for Frontend is missing or reshaped")
    else:
        stated = Counter({k: int(v) for k, v in re.findall(r"(\S+) x (\d+)", dist.group(2))})
        if int(dist.group(1)) != n:
            problems.append(f"License distribution says {dist.group(1)} frontend deps; the table has {n}")
        if stated != spdx:
            problems.append(f"License distribution for Frontend does not match the table. It should read: {want}")

    print(f"  package.json dependencies: {len(truth)}   frontend table: {n}")
    if not problems:
        print("OK: the frontend table names exactly what frontend/package.json ships, and its summary lines agree")
        return 0
    for p in problems:
        print(f"FAIL: {p}")
    return 1


if __name__ == "__main__":
    sys.exit(main())
