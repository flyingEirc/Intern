#!/usr/bin/env bash
# Accept and reject cases for cli-quality-stop-test.sh, with real processes.
# Every fixture runs in a session and process group of its own (setsid), so the
# cases use real group ids that are unrelated to this script's own group. They
# prove that the script stops exactly the recorded group -- including a group
# whose child ignores SIGTERM, which needs the SIGKILL escalation -- and leaves
# an unrelated group and this script alone. They also prove that a missing
# record is a normal no-op (a completed test) and that a corrupt record (the
# caller's own group, pid 1, garbage) is refused instead of becoming a
# host-wide kill.
#
# The last section generates mutants of the script and requires the cases to
# fail on each: a mutant that survives means the cases no longer protect that
# decision.
#
# STOP_TEST_SCRIPT points the cases at another script than the one next to this
# file.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script=${STOP_TEST_SCRIPT:-$here/cli-quality-stop-test.sh}
self=$here/$(basename "$0")

work=$(mktemp -d)
pgids=()
cleanup() {
	local pgid
	for pgid in ${pgids[@]+"${pgids[@]}"}; do
		kill -KILL -- "-$pgid" 2>/dev/null || true
	done
	rm -rf "$work"
}
trap cleanup EXIT

# The victim records its own pid, which is its group id: started through
# setsid --wait it is the session leader. ignore-term keeps the leader alive
# after SIGTERM; child-ignores lets the leader die and leaves a child that
# ignores SIGTERM, so only SIGKILL can end the group.
cat > "$work/stop-cases-victim" <<'VICTIM'
#!/usr/bin/env bash
set -u
printf '%s\n' "$$" > "$1"
case ${2:-} in
ignore-term) trap '' TERM ;;
child-ignores) bash -c 'trap "" TERM; while :; do sleep 0.2; done' & ;;
esac
while :; do sleep 0.2; done
VICTIM
cat > "$work/stop-cases-unrelated" <<'UNRELATED'
#!/usr/bin/env bash
set -u
printf '%s\n' "$$" > "$1"
while :; do sleep 0.2; done
UNRELATED
chmod +x "$work/stop-cases-victim" "$work/stop-cases-unrelated"

checks=0
failed=0
status=0
log=''
start_pgid=''

# start FIXTURE RECORD [MODE] starts FIXTURE in a session and process group of
# its own, waits until it recorded that group id and leaves it in start_pgid.
start() {
	local fixture=$1 record=$2 mode=${3:-} i
	rm -f "$record"
	setsid --wait "$work/stop-cases-$fixture" "$record" "$mode" > /dev/null 2>&1 &
	disown "$!" 2>/dev/null || true
	for ((i = 0; i < 100; i++)); do
		if [[ -s $record ]]; then
			start_pgid=$(<"$record")
			[[ $start_pgid =~ ^[0-9]+$ ]] && break
		fi
		sleep 0.05
	done
	if [[ ! $start_pgid =~ ^[0-9]+$ ]]; then
		echo "fixture $fixture did not record its process group" >&2
		exit 1
	fi
	pgids+=("$start_pgid")
}

# alive PGID reports whether a non-zombie process is still in the group.
alive() {
	local pgid=$1
	ps -e -o pgid=,stat= 2>/dev/null |
		awk -v group="$pgid" '$1 == group && $2 !~ /^Z/ { found = 1 } END { exit !found }'
}

# running PID reports whether PID is still a live (non-zombie) process. A group
# can outlive its leader through a child, so the unrelated fixture is checked by
# its leader: a name-based kill removes the leader and leaves only its orphan.
running() {
	local stat
	stat=$(ps -o stat= -p "$1" 2>/dev/null) || return 1
	[[ $stat != Z* ]]
}

# stop FILE runs the script as the workflow does and records status and log.
stop() {
	if log=$("$script" "$1" 2>&1); then
		status=0
	else
		status=$?
	fi
}

report() { # report NAME PROBLEM
	local name=$1 problem=${2:-}
	checks=$((checks + 1))
	if [[ -z $problem ]]; then
		printf 'ok   %s\n' "$name"
	else
		printf 'FAIL %s: %s\n%s\n' "$name" "$problem" "$log"
		failed=$((failed + 1))
	fi
}

# 1. A recorded live group is stopped with SIGTERM, the record goes away and the
# group is gone when the script returns.
file=$work/recorded.pgid
start victim "$file"
pgid=$start_pgid
stop "$file"
problem=''
if ((status != 0)); then problem="exit status $status, want 0"; fi
if [[ -z $problem && $log != *"stopping process group $pgid"* ]]; then problem="the log does not name group $pgid"; fi
if [[ -z $problem ]] && alive "$pgid"; then problem="group $pgid is still alive"; fi
if [[ -z $problem && -e $file ]]; then problem="the record was kept"; fi
report '1 a recorded group is stopped' "$problem"

# 2. A group whose leader dies on SIGTERM but whose child ignores it can only be
# stopped by the SIGKILL escalation.
file=$work/ignored.pgid
start victim "$file" child-ignores
pgid=$start_pgid
stop "$file"
problem=''
if ((status != 0)); then problem="exit status $status, want 0"; fi
if [[ -z $problem && $log != *"ignored SIGTERM; sending SIGKILL"* ]]; then problem="the log does not report the SIGKILL escalation"; fi
if [[ -z $problem ]] && alive "$pgid"; then problem="group $pgid survived the stop"; fi
if [[ -z $problem && -e $file ]]; then problem="the record was kept"; fi
report '2 a child ignoring SIGTERM needs, and gets, SIGKILL' "$problem"

# 3. Stopping the recorded group leaves an unrelated group and its record alone.
related=$work/related.pgid
start victim "$related"
related_pgid=$start_pgid
unrelated=$work/unrelated.pgid
start unrelated "$unrelated"
unrelated_pgid=$start_pgid
stop "$related"
problem=''
if ((status != 0)); then problem="exit status $status, want 0"; fi
if [[ -z $problem ]] && alive "$related_pgid"; then problem="the recorded group $related_pgid is still alive"; fi
if [[ -z $problem ]] && ! running "$unrelated_pgid"; then problem="the unrelated process $unrelated_pgid was stopped"; fi
if [[ -z $problem && ! -e $unrelated ]]; then problem="the unrelated record was removed"; fi
report '3 an unrelated process group is left alone' "$problem"

# 4. A record of the caller's own group is refused. The script under test runs
# in a session of its own so a script that ignores the guard can only kill that
# throwaway group, not this test or the rest of the host.
file=$work/own.pgid
if log=$(setsid --wait bash -c '
	own=$(ps -o pgid= -p $$ | tr -d " ")
	printf "%s\n" "$own" > "$1"
	"$2" "$1"
' _ "$file" "$script" 2>&1); then
	status=0
else
	status=$?
fi
problem=''
if ((status != 1)); then problem="exit status $status, want 1"; fi
if [[ -z $problem && $log != *"refusing to kill process group"* ]]; then problem="the log does not report the refusal"; fi
if [[ -z $problem && -e $file ]]; then problem="the record was kept"; fi
report "4 the caller's own group is refused" "$problem"

# 5. A pid below 2, or anything that is not a group id, is refused: a corrupt
# record must never become a host-wide kill.
problem=''
for record in 1 0 -5 garbage '12 13' ''; do
	file=$work/refused.pgid
	printf '%s\n' "$record" > "$file"
	stop "$file"
	if ((status != 1)); then problem=${problem:+$problem; }"record '$record': exit status $status, want 1"; fi
	if [[ $log != *"refusing to kill process group"* ]]; then problem=${problem:+$problem; }"record '$record': the log does not report the refusal"; fi
	if [[ -e $file ]]; then problem=${problem:+$problem; }"record '$record': the record was kept"; fi
done
report '5 pid 1, 0 and garbage records are refused' "$problem"

# 6. No record is the normal path after a completed test: nothing to stop and no
# error.
file=$work/missing.pgid
rm -f "$file"
stop "$file"
problem=''
if ((status != 0)); then problem="exit status $status, want 0"; fi
if [[ -z $problem && $log != *"no test process group recorded"* ]]; then problem="the log does not report the missing record"; fi
if [[ -z $problem && -e $file ]]; then problem="the script created a record"; fi
report '6 a missing record is a normal no-op' "$problem"

# 7. A group that already exited is a no-op, and its stale record goes away.
file=$work/exited.pgid
start victim "$file" ignore-term
pgid=$start_pgid
kill -KILL -- "-$pgid" 2>/dev/null || true
for ((i = 0; i < 100; i++)); do
	if ! alive "$pgid"; then break; fi
	sleep 0.05
done
stop "$file"
problem=''
if ((status != 0)); then problem="exit status $status, want 0"; fi
if [[ -z $problem && $log != *"already exited"* ]]; then problem="the log does not report the exited group"; fi
if [[ -z $problem && -e $file ]]; then problem="the record was kept"; fi
report '7 a group that already exited is a no-op' "$problem"

# 8. Mutants: each one must fail the case that protects its decision, with the
# expected assertion message. A survivor, or a failure for another reason (a
# crash, a fixture that could not start), means the cases stopped proving that
# decision.
mutant() {
	local name=$1 from=$2 to=$3 expected=$4 content mutated
	checks=$((checks + 1))
	content=$(<"$script")
	if [[ $content != *"$from"* ]]; then
		printf 'FAIL mutant %s: pattern not found: %s\n' "$name" "$from"
		failed=$((failed + 1))
		return
	fi
	mutated=${content//"$from"/"$to"}
	printf '%s\n' "$mutated" > "$work/mutant.sh"
	chmod +x "$work/mutant.sh"
	if STOP_TEST_SCRIPT=$work/mutant.sh STOP_CASES_MUTANTS=0 "$self" > "$work/mutant.log" 2>&1; then
		printf 'FAIL mutant survived: %s\n' "$name"
		failed=$((failed + 1))
	elif grep -Eq "$expected" "$work/mutant.log"; then
		printf 'ok   mutant killed: %s\n' "$name"
	else
		printf 'FAIL mutant %s was not caught by the expected case:\n' "$name"
		tail -n 5 "$work/mutant.log"
		failed=$((failed + 1))
	fi
}

if [[ ${STOP_CASES_MUTANTS:-1} == 1 ]]; then
	mutant 'host-wide kill by process name' \
		'kill -TERM -- "-$pgid" 2>/dev/null || true' \
		"pkill -TERM -f 'stop-cases-' 2>/dev/null || true" \
		'^FAIL 3 '
	mutant 'SIGKILL escalation removed' \
		'kill -KILL -- "-$pgid" 2>/dev/null || true' \
		':' \
		'^FAIL 2 '
	mutant 'own-group guard removed' \
		'[[ $pgid != "$own_pgid" ]] || refuse' \
		'true' \
		'^FAIL 4 '
	mutant 'missing-record no-op removed' \
		'if [[ ! -f $file ]]; then' \
		'if false; then' \
		'^FAIL 6 '
fi

printf '\n%s checks, %s failed\n' "$checks" "$failed"
[[ $failed -eq 0 ]]
