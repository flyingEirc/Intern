#!/usr/bin/env bash
set -euo pipefail

required=true
if [[ "${EVENT_NAME}" == "pull_request" || "${EVENT_NAME}" == "push" ]]; then
    if [[ "${BASE_SHA}" =~ ^[0-9a-f]{40}$ && "${BASE_SHA}" != 0000000000000000000000000000000000000000 ]]; then
        changes=$(mktemp)
        trap 'rm -f "$changes"' EXIT
        git diff --name-only -z "$BASE_SHA" "$HEAD_SHA" -- > "$changes"
        required=false
        while IFS= read -r -d '' path; do
            case "$path" in
                internal/*|app/*|go.*|Dockerfile|.dockerignore|THIRD_PARTY_NOTICES|mise.toml|.golangci.yml|.githooks/*|.github/*)
                    required=true; break ;;
                *.md)
                    ;; # Pure documentation is explicitly not a container-test pass.
                *) required=true; break ;;
            esac
        done < "$changes"
    fi
fi
matrix=$(jq -c '{profile: [.profiles[].id]}' app/cli/internal/qualitygate/contract.json)
printf 'required=%s\nmatrix=%s\n' "$required" "$matrix" >> "$GITHUB_OUTPUT"
printf 'Business/container checks required: %s\n' "$required"
