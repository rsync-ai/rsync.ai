#!/usr/bin/env bash
# No Go function in the four service modules may be unreachable from a main
# package unless scripts/deadcode-allowlist.txt names it with a reason.
#
# Why this exists: unreachable code is not free here. Every reader of the repo --
# people and the LLM agents that work on it -- takes a function's existence as
# evidence that something calls it. The first sweep (golang.org/x/tools deadcode)
# found 70 unreachable functions; 36 were deleted or moved into tests and the
# rest are the allowlist. Several were confidently wrong rather than merely
# unused: an anomaly "predictor" that predicted nothing, a sentinel Kafka
# identity the #1227 rewrite had orphaned, a query-result cache that would have
# held customer row values had anyone wired it in, and an Avro config default
# whose test covered a fiction. Each of them read as a live feature.
#
# How it works:
# - deadcode runs Rapid Type Analysis from every main package. It must see all
#   four modules in ONE workspace: api-gateway imports backend-orchestrator/pkg
#   through a `replace`, so analysing the orchestrator on its own reports its
#   pkg/ exports as dead when the gateway calls them. The go.work is generated
#   into a temp dir from the modules' own go.mod lines, never committed.
# - It runs WITHOUT -test. A function only tests call is dead in production, and
#   that is exactly what this reports; a seam that exists for tests belongs in a
#   _test.go file. (-test also builds every build-tagged test, which reports
#   false positives.)
# - Entries match on import path + function name, never line number, so moving a
#   function does not churn the allowlist.
# - The comparison runs both ways. A new unreachable function fails, and so does
#   an allowlist line whose function has since been called or deleted. That second
#   direction is the control: the 34 allowlisted entries are known-unreachable
#   functions, so a run of deadcode that silently found nothing (a broken filter,
#   a package pattern that matched no module) fails as 34 stale lines instead of
#   reporting a clean pass.
#
# Exit 0 = clean · 1 = a finding (each printed as a FAIL line) · anything else =
# setup error (no Go, deadcode did not install, a module did not type-check).
set -euo pipefail
cd "$(dirname "$0")/.."

DEADCODE_VERSION=v0.51.0
MODULES=(api-gateway backend-orchestrator backend-temporal-adapter connector-deployer)
ALLOWLIST=scripts/deadcode-allowlist.txt

fail=0
bad() { fail=1; printf '  FAIL  %s\n' "$*"; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# The workspace takes the highest `go` and `toolchain` any module declares;
# a lower one would refuse to load the module that needs the higher.
go_line=$(awk '$1 == "go" { print $2 }' "${MODULES[@]/%//go.mod}" | sort -V | tail -1)
toolchain=$(awk '$1 == "toolchain" { print $2 }' "${MODULES[@]/%//go.mod}" | sort -V | tail -1)
{
  printf 'go %s\n' "$go_line"
  [ -n "$toolchain" ] && printf 'toolchain %s\n' "$toolchain"
  printf 'use (\n'
  for m in "${MODULES[@]}"; do printf '\t%s/%s\n' "$PWD" "$m"; done
  printf ')\n'
} >"$tmp/go.work"
export GOTOOLCHAIN="${toolchain:-local}"

GOBIN="$tmp/bin" GOWORK=off go install "golang.org/x/tools/cmd/deadcode@$DEADCODE_VERSION"

# Report only the modules' own functions: a dependency's dead code is not ours.
filter=$(for m in "${MODULES[@]}"; do
  awk '$1 == "module" { print $2; exit }' "$m/go.mod" | sed 's/\./\\./g'
done | paste -sd'|' -)
filter="^($filter)(/|\$)"

pkgs=()
for m in "${MODULES[@]}"; do pkgs+=("./$m/..."); done

GOWORK="$tmp/go.work" "$tmp/bin/deadcode" -filter "$filter" \
  -f '{{range .Funcs}}{{$.Path}} {{.Name}}	{{.Position.File}}:{{.Position.Line}}{{"\n"}}{{end}}' \
  "${pkgs[@]}" >"$tmp/raw.txt"

sed "s#	$PWD/#	#" "$tmp/raw.txt" | LC_ALL=C sort >"$tmp/found_at.txt"
cut -f1 "$tmp/found_at.txt" | LC_ALL=C sort -u >"$tmp/found.txt"
grep -v -e '^[[:space:]]*#' -e '^[[:space:]]*$' "$ALLOWLIST" | sed 's/[[:space:]]*$//' |
  LC_ALL=C sort -u >"$tmp/allowed.txt"

echo "deadcode $DEADCODE_VERSION over ${MODULES[*]} (GOTOOLCHAIN=$GOTOOLCHAIN)"

while IFS= read -r key; do
  where=$(awk -F'\t' -v k="$key" '$1 == k { print $2; exit }' "$tmp/found_at.txt")
  bad "unreachable and not allowlisted: $key ($where)"
done < <(LC_ALL=C comm -23 "$tmp/found.txt" "$tmp/allowed.txt")

while IFS= read -r key; do
  bad "allowlisted but no longer unreachable -- delete the line from $ALLOWLIST: $key"
done < <(LC_ALL=C comm -13 "$tmp/found.txt" "$tmp/allowed.txt")

if [ "$fail" -ne 0 ]; then
  cat <<EOF

A function nothing in a main package can reach. In order of preference:
  1. delete it (and its test, if only the test called it);
  2. if only tests need it, move it into a _test.go file;
  3. if it is parked on purpose, add "<import path> <name>" to $ALLOWLIST
     under a comment saying why and what would retire it.
A stale allowlist line just needs deleting.
EOF
  exit 1
fi

printf '  ok    %d unreachable, all %d allowlisted; nothing else unreachable\n' \
  "$(wc -l <"$tmp/found.txt" | tr -d ' ')" "$(wc -l <"$tmp/allowed.txt" | tr -d ' ')"
