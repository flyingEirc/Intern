#!/usr/bin/env bash
# Removes the given images and, with each of them, the ancestor images nothing
# else needs: an ancestor stays while another image still has it as its parent,
# while it carries a tag or a digest reference, and while a container uses it.
#
# Why not `docker image rm`. The Docker CLI asks the daemon to prune: it also
# deletes every untagged ancestor of the removed image. The classic builder that
# testcontainers uses shares those ancestors between all builds of the same
# Dockerfile (they are its build cache, "Using cache"), and since Docker 29 the
# containerd image store deletes them without asking whether another image or a
# running build still depends on them (moby daemon/containerd/image_delete.go,
# deleteAll: "Should this also check parentage here?"). A build of another job
# that is committing a step onto such an ancestor then fails with "failed to
# export image: No such image: sha256:..." (issue #70). This script never
# prunes: it removes the image itself without pruning, then walks the parent
# chain recorded before the removal and removes an ancestor only when no other
# image has it as its parent. An image that stays is not an error, and neither
# is an image that is already gone: other jobs run the same cleanup.
#
#   remove-images.sh REF...
#
# Exit status is non-zero when the daemon cannot be reached or a removal failed
# for another reason than an image being in use or already gone.
set -euo pipefail

if (($# == 0)); then
	echo 'usage: remove-images.sh REF...' >&2
	exit 2
fi

id_format='{{.Id}}'
# One line per image: id, parent (- for none), number of tags, number of
# digest references. Both counts use index so an image without the field is 0.
graph_format='{{.Id}} {{with index . "Parent"}}{{.}}{{else}}-{{end}} {{with index . "RepoTags"}}{{len .}}{{else}}0{{end}} {{with index . "RepoDigests"}}{{len .}}{{else}}0{{end}}'

err=$(mktemp)
trap 'rm -f "$err"' EXIT

# remove_one TARGET removes one reference or image id without pruning. It
# returns 0 when the image went or was already gone, 3 when a container uses it
# and 1 for any other failure.
remove_one() {
	if docker image rm --no-prune "$1" >/dev/null 2>"$err"; then
		return 0
	fi
	if grep -q 'No such image' "$err"; then
		return 0
	fi
	if grep -q 'conflict:' "$err"; then
		return 3
	fi
	cat "$err" >&2
	return 1
}

# ancestors ID prints the ancestors of ID, nearest first, that are removable once
# ID itself is gone, read from the graph lines on stdin.
ancestors() {
	awk -v target="$1" '
		{ parent[$1] = $2; refs[$1] = $3 + $4; if ($2 != "-") children[$2]++ }
		END {
			if (!(target in parent)) exit
			current = parent[target]
			if (current != "-") children[current]--
			while (current != "-" && (current in parent) && refs[current] == 0 && children[current] == 0) {
				print current
				next_ancestor = parent[current]
				if (next_ancestor != "-") children[next_ancestor]--
				current = next_ancestor
			}
		}'
}

status=0
for ref in "$@"; do
	if ! id=$(docker image inspect --format "$id_format" "$ref" 2>"$err"); then
		if grep -q 'No such image' "$err"; then
			echo "remove-images: $ref is already gone"
		else
			cat "$err" >&2
			status=1
		fi
		continue
	fi

	# The parent links are read before the removal, from one snapshot of every
	# image. An image another job deletes between the listing and the inspection
	# is missing from it, which is harmless; any other error is not.
	listed=$(docker image ls --all --quiet --no-trunc | LC_ALL=C sort -u)
	graph=''
	if [[ -n $listed ]]; then
		# shellcheck disable=SC2086 # ids are newline separated by design
		if ! graph=$(docker image inspect --format "$graph_format" $listed 2>"$err") && [[ -n $(grep -v 'No such image' "$err" || true) ]]; then
			cat "$err" >&2
			status=1
			continue
		fi
	fi

	code=0
	remove_one "$ref" || code=$?
	if ((code == 3)); then
		echo "remove-images: kept $ref: a container uses it"
		continue
	elif ((code != 0)); then
		status=1
		continue
	fi
	if docker image inspect --format "$id_format" "$id" >/dev/null 2>&1; then
		echo "remove-images: untagged $ref; image ${id#sha256:} stays, another tag, a child or a container needs it"
		continue
	fi
	echo "remove-images: removed $ref"

	for ancestor in $(printf '%s\n' "$graph" | ancestors "$id"); do
		code=0
		remove_one "$ancestor" || code=$?
		if ((code == 3)); then
			echo "remove-images: kept ancestor ${ancestor#sha256:}: a container uses it"
			break
		elif ((code != 0)); then
			status=1
			break
		fi
		echo "remove-images: removed ancestor ${ancestor#sha256:} of $ref, no other image used it"
	done
done
exit "$status"
