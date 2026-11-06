#!/bin/sh
# Regenerates the OpenAPI client from app/web/api/openapi.yaml and fails when
# the result differs from the checked-in copy: a contract change must ship
# with its regenerated client. Works without git (the pre-commit hook runs it
# in an exported snapshot) and leaves the tree as it found it.
set -eu
cd "$(dirname "$0")/.."
generated=lib/api/generated
before=$(mktemp -d "${TMPDIR:-/tmp}/intern-generated.XXXXXX")
trap 'rm -rf "$before"' EXIT HUP INT TERM
cp -R "$generated" "$before/generated"
bunx --bun openapi-ts > /dev/null
if ! diff -r "$before/generated" "$generated"; then
    rm -rf "$generated"
    cp -R "$before/generated" "$generated"
    echo "generate:check: $generated is out of date with ../api/openapi.yaml; run 'bun run generate' and commit the result" >&2
    exit 1
fi
echo "generate:check: $generated matches ../api/openapi.yaml"
