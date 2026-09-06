#!/bin/sh
# Finds template overrides whose upstream original changed since the merge
# base — the failure git cannot flag, because an override never conflicts.
#
# Every screen breakage after an upstream merge in this fork's history came
# from exactly this: the copy under custom/templates kept calling a helper or
# field the original had renamed, the compiler had nothing to say about it,
# and the page failed at render time. Run this after every `git merge
# upstream/main`, and eyeball the diff of anything it prints.
#
# Usage: docs/company/scripts/check-template-drift.sh [upstream-ref]
set -e
UPSTREAM="${1:-upstream/main}"
BASE=$(git merge-base HEAD "$UPSTREAM")

found=0
for f in $(cd custom/templates && find . -name '*.tmpl' | sed 's|^\./||'); do
    # Overrides of an upstream file only: company/* has no original to drift from.
    [ -f "templates/$f" ] || continue
    if ! git diff --quiet "$BASE" "$UPSTREAM" -- "templates/$f" 2>/dev/null; then
        echo "CHANGED UPSTREAM: templates/$f  (we override it — diff and re-apply)"
        found=1
    fi
done

if [ "$found" = 0 ]; then
    echo "ok: no overridden template changed upstream since $(git rev-parse --short "$BASE")"
fi
exit $found
