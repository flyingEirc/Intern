#!/usr/bin/env bash
# Fails unless this self-hosted runner has what the jobs assume, naming everything
# that is missing in one message: a Linux x86_64 host, the host tools the gates
# call and, per flag, a C compiler for the race detector and a native Linux amd64
# Docker engine. The real component gates must block on a missing or mismatched
# runtime; they never skip and never fall back to mocks (docs/guide/quality.md),
# so a runner without the engine gets one explicit failure here instead of a
# dozen opaque ones later.
#
#   require-runner.sh [--cc] [--docker] [--compose]
#     --cc       a C compiler (go test -race needs cgo)
#     --docker   the Docker CLI reaches a native linux/amd64 engine
#     --compose  --docker plus the Compose plugin and a kernel that can apply the
#                cgroup I/O weight compose.yaml asks for (the D1 deployment job)
set -euo pipefail

want_cc=0 want_docker=0 want_compose=0
for arg in "$@"; do
	case "$arg" in
	--cc) want_cc=1 ;;
	--docker) want_docker=1 ;;
	--compose) want_docker=1 want_compose=1 ;;
	*)
		echo 'usage: require-runner.sh [--cc] [--docker] [--compose]' >&2
		exit 2
		;;
	esac
done

problems=()
need() { problems+=("$*"); }

[[ "$(uname -s)" == Linux ]] ||
	need "the host is $(uname -s); this workflow runs on a Linux runner (docs/guide/quality.md, CI execution environment)"
[[ "$(uname -m)" == x86_64 ]] ||
	need "the host is $(uname -m); the gates need a linux/amd64 host"

for tool in git jq curl tar sha256sum timeout; do
	command -v "$tool" >/dev/null 2>&1 || need "$tool is not installed"
done

# The container-mysql test step starts `go test | tee` in a session and process
# group of its own through setsid, and the stop step and its cases read live
# process groups with ps (procps).
if ! command -v setsid >/dev/null 2>&1; then
	need 'setsid is not installed (util-linux): the container-mysql test step starts its test pipeline in a session and process group with it'
fi
if ! command -v ps >/dev/null 2>&1; then
	need 'ps is not installed (procps): the container-mysql stop step and its cases find the recorded process group with it'
fi

if ((want_cc)) && ! command -v cc >/dev/null 2>&1 && ! command -v gcc >/dev/null 2>&1; then
	need 'no C compiler (cc or gcc): go test -race needs cgo; install build-essential'
fi

# docker_gate runs the Docker CLI under a deadline and stores its combined output
# and exit status in docker_output/docker_status. A hung daemon must fail this
# check in seconds, not hold the job until its own timeout: GNU timeout sends
# SIGTERM at the deadline and SIGKILL 5 s later (-k), which reports 124 or 137.
docker_output='' docker_status=0
docker_gate() {
	local seconds=$1
	shift
	docker_status=0
	docker_output=$(timeout -k 5 "$seconds" "$@" 2>&1) || docker_status=$?
}
docker_timed_out() { ((docker_status == 124 || docker_status == 137)); }

if ((want_docker)); then
	if ! command -v docker >/dev/null 2>&1; then
		need 'docker is not installed (Docker Engine from the Docker apt repository, not only a Docker Desktop shim)'
	else
		docker_gate 30 docker version --format '{{.Server.Os}}/{{.Server.Arch}}'
		platform=$docker_output
		platform=${platform//$'\n'/ }
		if docker_timed_out; then
			need "Docker daemon not responding within 30 s (docker version): ${platform:-(no output)} -> fix: restart the engine (sudo systemctl restart docker); no gate can run against a hung daemon"
		elif ((docker_status != 0)); then
			hint=''
			case "$platform" in
			*'permission denied'*)
				hint=' -> fix: add the runner account to the docker group (sudo usermod -aG docker <account>) and restart the runner service so the new group applies'
				;;
			*'Cannot connect'* | *'no such file'*)
				hint=' -> fix: start the engine (sudo systemctl enable --now docker); in WSL2 systemd must be enabled in /etc/wsl.conf'
				;;
			esac
			need "the Docker engine is not reachable by $(id -un): ${platform}${hint}"
		else
			[[ "$platform" == linux/amd64 ]] ||
				need "the Docker engine reports ${platform}; the gates accept only a native linux/amd64 engine"
			if ((want_compose)); then
				docker compose version >/dev/null 2>&1 ||
					need 'the Docker Compose plugin is not installed (docker-compose-plugin)'
				# compose.yaml sets blkio_config.weight. runc applies it by writing io.bfq.weight
				# (or io.weight) in the container's cgroup, so the kernel has to provide one of
				# them. A kernel without it (the WSL2 default: its bfq is a module that is not
				# loaded) fails `compose up` only after the image build, so probe it here, with
				# the MySQL image the D1 job pulls anyway.
				if [[ "$platform" == linux/amd64 ]] && ((${#problems[@]} == 0)); then
					compat=$(dirname "$0")/../../internal/physical/compat.json
					probe_image=$(jq -r '.[0].mysql_image // empty' "$compat" 2>/dev/null || true)
					if [[ -z "$probe_image" ]]; then
						need "no mysql_image in ${compat}"
					elif ! docker image inspect "$probe_image" >/dev/null 2>&1 && ! docker pull --quiet "$probe_image" >/dev/null 2>&1; then
						need "cannot pull ${probe_image} (internal/physical/compat.json), which the D1 job needs anyway"
					else
						docker_gate 120 docker run --rm --blkio-weight=300 --entrypoint true "$probe_image"
						if docker_timed_out; then
							need "Docker daemon not responding within 120 s (docker run probe) -> fix: restart the engine (sudo systemctl restart docker)"
						elif ((docker_status != 0)); then
							probe=$docker_output
							probe=${probe//$'\n'/ }
							need "the engine cannot apply a cgroup I/O weight, which compose.yaml's blkio_config.weight needs: ${probe} -> fix: the kernel must provide io.bfq.weight or io.weight; on the WSL2 kernel run 'sudo modprobe bfq' and put 'bfq' in /etc/modules-load.d/bfq.conf"
						fi
					fi
				fi
			fi
		fi
	fi
fi

if ((${#problems[@]})); then
	for problem in "${problems[@]}"; do
		echo "::error title=Runner prerequisite missing::${problem}"
	done
	exit 1
fi

# Facts for the job log; not a gate.
echo "Runner: $(uname -srm), $(nproc) CPUs, $(awk '/^MemTotal/ { printf "%d MiB", $2 / 1024 }' /proc/meminfo) RAM, $(df -h --output=avail . | tail -n 1 | tr -d ' ') free in $(pwd)"
if ((want_docker)); then
	docker_gate 30 docker info --format 'Docker engine: {{.OSType}}/{{.Architecture}} {{.ServerVersion}}, {{.NCPU}} CPUs, {{.MemTotal}} bytes, {{.OperatingSystem}}, root {{.DockerRootDir}}'
	if docker_timed_out; then
		echo "::error title=Runner prerequisite missing::Docker daemon not responding within 30 s (docker info)"
		exit 1
	elif ((docker_status != 0)); then
		echo "::error title=Runner prerequisite missing::docker info failed: ${docker_output//$'\n'/ }"
		exit 1
	fi
	echo "$docker_output"
fi
