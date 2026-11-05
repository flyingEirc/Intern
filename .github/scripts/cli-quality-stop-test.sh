#!/usr/bin/env bash
# Stops exactly the process group recorded for this job's CLI integration test,
# and nothing else. The test step starts `go test | tee` in a session and process
# group of its own, and the group leader records that group id in a file under
# RUNNER_TEMP. After a cancellation the step shell is gone but the group lives
# on as an orphan until the job end SIGKILLs it, racing the evidence check and
# the post-clean/inventory steps; this script sends SIGTERM to that one group,
# escalates to SIGKILL after a short grace period, and removes the record. A
# missing record is the normal path after a completed test: there is nothing to
# stop. A record of the caller's own group, of a pid below 2, or of anything
# that is not a group id is refused: a corrupt file must never turn into a
# host-wide kill.
#
#   cli-quality-stop-test.sh <pgid-file>
set -euo pipefail

file=${1:?usage: cli-quality-stop-test.sh <pgid-file>}
if [[ ! -f $file ]]; then
	echo "cli-quality-stop-test: no test process group recorded"
	exit 0
fi
pgid=$(<"$file")
own_pgid=$(ps -o pgid= -p $$ | tr -d ' ')
refuse() {
	echo "cli-quality-stop-test: refusing to kill process group '$pgid' recorded in $file" >&2
	rm -f "$file"
	exit 1
}
# At most nine digits: a longer record cannot be a pid, and the leading zeros are
# dropped so that "0042" cannot pass the own-group check as a string and then be
# signalled as group 42.
[[ $pgid =~ ^[0-9]{1,9}$ ]] || refuse
pgid=$((10#$pgid))
((pgid > 1)) || refuse
[[ $pgid != "$own_pgid" ]] || refuse

# Only live processes matter: a zombie cannot run, cannot be signalled into
# stopping, and is reaped by its new parent once the group leader is gone.
group_alive() {
	local table
	if table=$(ps -e -o pgid=,stat= 2>/dev/null); then
		printf '%s\n' "$table" | awk -v group="$pgid" '$1 == group && $2 !~ /^Z/ { found = 1 } END { exit !found }'
	else
		kill -0 -- "-$pgid" 2>/dev/null
	fi
}
wait_gone() { # wait_gone SECONDS
	local deadline=$1 i
	for ((i = 0; i < deadline * 10; i++)); do
		group_alive || return 0
		sleep 0.1
	done
	! group_alive
}

if ! group_alive; then
	echo "cli-quality-stop-test: process group $pgid already exited"
	rm -f "$file"
	exit 0
fi

echo "cli-quality-stop-test: stopping process group $pgid"
kill -TERM -- "-$pgid" 2>/dev/null || true
if wait_gone 5; then
	rm -f "$file"
	exit 0
fi
echo "cli-quality-stop-test: process group $pgid ignored SIGTERM; sending SIGKILL"
kill -KILL -- "-$pgid" 2>/dev/null || true
if wait_gone 5; then
	rm -f "$file"
	exit 0
fi
echo "cli-quality-stop-test: process group $pgid survived SIGKILL (uninterruptible process); the job end will reap it" >&2
rm -f "$file"
exit 1
