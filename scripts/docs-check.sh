#!/bin/sh
# docs-check.sh - the documentation drift gates that used to be a paragraph in docs/README.md.
#
# Three failures this catches, all of which actually happened in this repository:
#   1. a link to a doc that was renamed or never existed (architecture.html -> architecture-map.html)
#   2. a count in the index that stopped matching the record directories (9 ADRs while 10 existed)
#   3. a row added to roadmap.md but not to roadmap.html (the mirror is manual)
#
#   sh scripts/docs-check.sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
fail=0

note_fail() { echo "docs-check: $*" >&2; fail=1; }

# ---------------------------------------------------------------------------
# 1. every docs/<name>.md|html reference resolves
# ---------------------------------------------------------------------------
refs=$(grep -rho --exclude-dir=.git --include='*.md' --include='*.html' \
  -E 'docs/[a-z0-9-]+\.(md|html)' . | sort -u)
for ref in $refs; do
  [ -e "$ref" ] || note_fail "reference to '$ref' does not resolve"
done

# ---------------------------------------------------------------------------
# 2. counts in the index match the directories
# ---------------------------------------------------------------------------
specs=$(find .harness/specs -name '*.spec.md' ! -name '_TEMPLATE.spec.md' | wc -l | tr -d ' ')
adrs=$(find .harness/adr -name '*.md' ! -name '_TEMPLATE.adr.md' | wc -l | tr -d ' ')
rules=$(find .harness/rules -name '*.md' | wc -l | tr -d ' ')

# The counts live on two lines of docs/README.md: specs and ADRs share the "records" row,
# rules has its own. Reading them in order is less fragile than one clever regexp.
specs_line=$(grep -F '.harness/specs/' docs/README.md | head -n 1)
said_specs=$(printf '%s' "$specs_line" | grep -o '[0-9][0-9]* 份' | sed -n '1s/ .*//p')
said_adrs=$(printf '%s' "$specs_line" | grep -o '[0-9][0-9]* 份' | sed -n '2s/ .*//p')
rules_line=$(grep -F '.harness/rules/' docs/README.md | head -n 1)
said_rules=$(printf '%s' "$rules_line" | grep -o '[0-9][0-9]* 份' | sed -n '1s/ .*//p')

[ -z "$said_specs" ] || [ "$said_specs" = "$specs" ] ||
  note_fail "docs/README.md says $said_specs specs, the directory has $specs"
[ -z "$said_adrs" ] || [ "$said_adrs" = "$adrs" ] ||
  note_fail "docs/README.md says $said_adrs ADRs, the directory has $adrs"
[ -z "$said_rules" ] || [ "$said_rules" = "$rules" ] ||
  note_fail "docs/README.md says $said_rules rules, the directory has $rules"
# ---------------------------------------------------------------------------
# 3. roadmap.md and roadmap.html agree on the sections that change often
# ---------------------------------------------------------------------------
# `^| ` matches the header and data rows but not the `|---|` separator, so one is subtracted.
md_rows() { sed -n "/^## $2\./,/^## $3\./p" "$1" | grep -c '^| ' | awk '{ print ($1 > 0 ? $1 - 1 : 0) }'; }
html_rows() { sed -n "/<h2>$2\./,/<h2>$3\./p" "$1" | grep -o '<tr><td>' | wc -l | tr -d ' '; }

md4=$(md_rows docs/roadmap.md 4 5)
ht4=$(html_rows docs/roadmap.html 4 5)
[ "$md4" = "$ht4" ] || note_fail "roadmap §4: $md4 rows in .md but $ht4 in .html"

md5=$(md_rows docs/roadmap.md 5 6)
ht5=$(html_rows docs/roadmap.html 5 6)
[ "$md5" = "$ht5" ] || note_fail "roadmap §5: $md5 rows in .md but $ht5 in .html"

# §6 is three tables under one heading, so three headers are subtracted instead of one.
md6=$(sed -n '/^## 6\./,/^## 7\./p' docs/roadmap.md | grep -c '^| ' | awk '{ print ($1 > 0 ? $1 - 3 : 0) }')
ht6=$(sed -n '/<h2>6\./,/<h2>7\./p' docs/roadmap.html | grep -o '<tr><td>' | wc -l | tr -d ' ')
[ "$md6" = "$ht6" ] || note_fail "roadmap §6: $md6 rows in .md but $ht6 in .html"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
printf 'docs-check: OK (%s specs, %s ADRs, %s rules; roadmap §4 %s/%s, §5 %s/%s, §6 %s/%s rows)\n' \
  "$specs" "$adrs" "$rules" "$md4" "$ht4" "$md5" "$ht5" "$md6" "$ht6"