#!/usr/bin/env bash
# Keeps the newest few images of every Web fixture on this Docker host and
# removes the superseded ones. The Web acceptance harness (app/web/integration,
# image_test.go) builds each fixture from its Dockerfile under a tag that is
# determined by the Dockerfile, the files it copies, the build arguments and the
# architecture, intern-quality-web:<fixture>-<12 hex digits>-<arch>, followed
# inside GitHub Actions by -<8 hex digits> naming the runner (the digest prefix
# of its label), and never removes it: every job reuses the image its own inputs
# name. The runner part keeps two runners that build the same fixture at the
# same time, with different image ids, from replacing one tag at once, which
# Docker's containerd image store answers with AlreadyExists. Without this step
# a changed Dockerfile or pin would leave its old image on the host for good.
#
# Only that repository and that tag shape are considered. The newest
# keep_images of each fixture, architecture and runner by creation time stay, so
# jobs of a few commits or branches that are running at the same time still find
# their image. An older image goes through remove-images.sh, which removes the
# image and the ancestors nothing else needs and never anything a container or
# another image still uses (see there why `docker image rm` is not used).
#
#   web-image-clean.sh
#
# Needs docker. Exit status is non-zero when the daemon cannot be reached or a
# removal failed; a removal failure does not keep the other images.
set -euo pipefail

if (($# != 0)); then
	echo 'usage: web-image-clean.sh' >&2
	exit 2
fi

repository=intern-quality-web
# Images kept per fixture, architecture and runner.
keep_images=3
tag_pattern='^([a-z0-9]+(-[a-z0-9]+)*)-([0-9a-f]{12})-(amd64|arm64)(-([0-9a-f]{8}))?$'
here=$(cd "$(dirname "$0")" && pwd)
# REMOVE_IMAGES points the cases at a stand-in for remove-images.sh.
remove_images=${REMOVE_IMAGES:-$here/remove-images.sh}

# The listing is its own assignment: a failing docker must end the script, not
# look like an empty host.
listed=$(docker image ls --filter "reference=$repository:*" --format '{{.CreatedAt}} {{.Repository}}:{{.Tag}}')

# One line per fixture image: fixture, architecture and runner, creation time
# and reference, separated by tabs, so that sorting puts the newest of each
# group first.
tab=$'\t'
entries=''
while IFS= read -r entry; do
	[[ -n $entry ]] || continue
	ref=${entry##* }
	created=${entry% *}
	[[ $ref == "$repository:"* ]] || continue
	[[ ${ref#"$repository:"} =~ $tag_pattern ]] || continue
	entries="$entries${BASH_REMATCH[1]}/${BASH_REMATCH[4]}/${BASH_REMATCH[6]}$tab$created$tab$ref"$'\n'
done <<< "$listed"

superseded=()
previous=''
rank=0
while IFS=$tab read -r group created ref; do
	[[ -n $ref ]] || continue
	if [[ $group != "$previous" ]]; then
		previous=$group
		rank=0
	fi
	rank=$((rank + 1))
	if ((rank <= keep_images)); then
		echo "web-image-clean: keeping fixture image $ref"
	else
		echo "web-image-clean: removing superseded fixture image $ref"
		superseded+=("$ref")
	fi
done <<< "$(printf '%s' "$entries" | LC_ALL=C sort -t "$tab" -k1,1 -k2,2r)"

status=0
if ((${#superseded[@]} > 0)); then
	"$remove_images" "${superseded[@]}" || status=1
elif [[ -z $entries ]]; then
	echo "web-image-clean: no Web fixture images on this host"
fi
exit "$status"
