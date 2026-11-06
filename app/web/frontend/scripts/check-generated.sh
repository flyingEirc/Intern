#!/bin/sh
# Regenerates the OpenAPI client from app/web/api/openapi.yaml and fails when
# the result differs from the checked-in copy: a contract change must ship
# with its regenerated client. Works without git (the pre-commit hook runs it
# in an exported snapshot) and leaves the tree as it found it.
set -eu
cd "$(dirname "$0")/.."
contract=../api/openapi.yaml
generated=lib/api/generated
# openapi-ts 0.99.0 exits 0 when the input is missing or unparsable, so both
# conditions are checked here instead of trusting its status.
if [ ! -s "$contract" ]; then
    echo "generate:check: $contract is missing or empty" >&2
    exit 1
fi
before=$(mktemp -d "${TMPDIR:-/tmp}/intern-generated.XXXXXX")
trap 'rm -rf "$before"' EXIT HUP INT TERM
cp -R "$generated" "$before/generated"
rm -f openapi-ts-error-*.log
bunx --bun openapi-ts > /dev/null
if ls openapi-ts-error-*.log > /dev/null 2>&1; then
    cat openapi-ts-error-*.log >&2
    rm -f openapi-ts-error-*.log
    rm -rf "$generated"
    cp -R "$before/generated" "$generated"
    echo "generate:check: openapi-ts could not generate from $contract" >&2
    exit 1
fi
if ! diff -r "$before/generated" "$generated"; then
    rm -rf "$generated"
    cp -R "$before/generated" "$generated"
    echo "generate:check: $generated is out of date with $contract; run 'bun run generate' and commit the result" >&2
    exit 1
fi
echo "generate:check: $generated matches $contract"
