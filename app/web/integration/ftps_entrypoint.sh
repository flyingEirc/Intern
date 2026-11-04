#!/bin/sh
set -eu
umask 077
chmod 0711 /data

# The FTPS fixture answers on six listeners at once, each behind its own audit
# tap. The tap is what proves, on the server's side of the wire, that a refused
# session never carried a credential: it forwards the bytes and keeps only counts.
#
# Every daemon therefore binds the container's own routable address, never
# loopback: Pure-FTPd creates a passive data socket on the *local* endpoint of the
# control connection, so a daemon that only listened on 127.0.0.1 would advertise
# a data port no worker container can reach.
ADVERTISE=$(ip -4 -o addr show scope global | awk 'NR==1 { split($4, a, "/"); print a[1] }')
if [ -z "${ADVERTISE:-}" ]; then
    echo "no global IPv4 address to bind the fixture daemons with" >&2
    exit 1
fi

for required in /run/s2-password /run/s2/marker /usr/local/bin/s2-tap \
    /run/s2/good.pem /run/s2/wronghost.pem /run/s2/expired.pem /run/s2/rogue.pem; do
    if [ ! -s "$required" ]; then
        echo "missing fixture input: $required" >&2
        exit 1
    fi
done

mkdir -p /run/pure-ftpd /audit
chmod 0755 /audit

# One real account, the same musl bcrypt backend the plain fixture uses. The
# password is read from the injected root-only file twice, as pure-pw requires,
# and never appears in an argument, in the environment or in a log.
pure-pw useradd normal -f /data/users.txt -u 1000 -g 1000 -d /data/normal < /run/s2-password
pure-pw mkdb /data/users.pdb -f /data/users.txt
chmod 0600 /data/users.txt /data/users.pdb
: > /run/s2-auth-backend
while IFS=: read -r account password_hash rest; do
    case "$account" in
        normal) ;;
        *) echo "unexpected fixture account" >&2; exit 1 ;;
    esac
    case "$password_hash" in
        '$2a$10$'*) ;;
        *) echo "unexpected fixture authentication backend" >&2; exit 1 ;;
    esac
    test "${#password_hash}" -eq 60
    printf '%s bcrypt cost=10\n' "$account" >> /run/s2-auth-backend
done < /data/users.txt
unset password_hash rest

# The trust anchors the workers pin. A CA certificate is public material, so the
# fixture republishes it on the shared audit volume instead of smuggling it
# through an environment variable. The private CA key is never in this container.
for bundle in ca rogue-ca; do
    if [ -s "/run/s2/$bundle.pem" ]; then
        cp "/run/s2/$bundle.pem" "/audit/$bundle.pem"
        chmod 0644 "/audit/$bundle.pem"
    fi
done

export S2_TAP_SECRET=$(awk 'NR==1' /run/s2-password)
export S2_TAP_MARKER=$(awk 'NR==1' /run/s2/marker)
PIDS=''

start_listener() {
    # name client_port daemon_port passive_first passive_last tls_level cert daemon
    name=$1
    client=$2
    daemon=$3
    first=$4
    last=$5
    level=$6
    cert=$7
    daemon_binary=$8
    S2_TAP_NAME="$name" S2_TAP_LISTEN="0.0.0.0:$client" S2_TAP_DIAL="$ADVERTISE:$daemon" \
        S2_TAP_AUDIT=/audit/control.jsonl /usr/local/bin/s2-tap &
    PIDS="$PIDS $!"
    case "$level" in
        0)
            # A server without TLS at all: compiled with TLS support, but the
            # upgrade is refused, so this is the downgrade oracle.
            # -w (allowuserfxp): the control channel reaches the daemon through
            # the s2-tap relay, so its peer is this container, while PASV data
            # connections come straight from the worker; without -w Pure-FTPd
            # drops every data connection whose address differs from the control
            # peer. It changes no TLS behaviour.
            "$daemon_binary" -H -S"$ADVERTISE,$daemon" -lpuredb:/data/users.pdb -E -A -j -w \
                -u1000 -p"$first:$last" -c64 -C64 -g"/run/pure-ftpd/$name.pid" -Y 0 &
            ;;
        *)
            "$daemon_binary" -H -S"$ADVERTISE,$daemon" -lpuredb:/data/users.pdb -E -A -j -w \
                -u1000 -p"$first:$last" -c64 -C64 -g"/run/pure-ftpd/$name.pid" \
                -Y "$level" --certfile "/run/s2/$cert.pem" &
            ;;
    esac
    PIDS="$PIDS $!"
    printf '%s %s %s %s %s %s %s\n' "$name" "$client" "$daemon" "$first" "$last" "$level" "$cert" \
        >> /run/s2/listeners
}

: > /run/s2/listeners
start_listener explicit  2121 12121 50100 50119 3 good /usr/sbin/pure-ftpd
# The plain face is started with no --certfile at all (level 0 refuses AUTH TLS),
# so it must publish cert=none: a manifest that claimed a certificate this daemon
# never loads would be the fixture lying about the very downgrade oracle it is
# there to provide, and s2fixture.MatchesMatrix() would accept a drifted listener.
start_listener plain     2122 12122 50200 50219 0 none /usr/sbin/pure-ftpd
start_listener wronghost 2123 12123 50300 50319 3 wronghost /usr/sbin/pure-ftpd
start_listener expired   2124 12124 50400 50419 3 expired /usr/sbin/pure-ftpd
start_listener rogue      2125 12125 50500 50519 3 rogue /usr/sbin/pure-ftpd
# The implicit listener is the second compile of the same pinned source, with
# IMPLICIT_TLS defined: TLS from the first byte, and the data protection level
# fixed to private before a single command is read.
start_listener implicit  990 1990 50600 50619 3 good /usr/local/sbin/pure-ftpd-implicit

# Wait until every daemon listens on its own address and every tap answers, so a
# container that reports readiness has a complete fixture behind it.
i=0
while [ "$i" -lt 60 ]; do
    ready=1
    while read -r name client daemon first last level cert; do
        nc -z "$ADVERTISE" "$daemon" >/dev/null 2>&1 || ready=0
        nc -z 127.0.0.1 "$client" >/dev/null 2>&1 || ready=0
    done < /run/s2/listeners
    if [ "$ready" -eq 1 ]; then
        break
    fi
    i=$((i + 1))
    sleep 1
done
if [ "$ready" != 1 ]; then
    echo "the FTPS fixture did not come up within 60 seconds" >&2
    exit 1
fi

# The manifest is the fixture's own account of what it is. A worker that reports a
# listener, a TLS level or a certificate digest reads it back from here instead of
# restating what it hoped the fixture would be.
{
    printf 'advertise=%s\n' "$ADVERTISE"
    printf 'daemon_sha256=%s\n' "$(sha256sum /usr/sbin/pure-ftpd | cut -d' ' -f1)"
    printf 'implicit_daemon_sha256=%s\n' "$(sha256sum /usr/local/sbin/pure-ftpd-implicit | cut -d' ' -f1)"
    printf 'tap_sha256=%s\n' "$(sha256sum /usr/local/bin/s2-tap | cut -d' ' -f1)"
    printf 'auth_backend=%s\n' "$(tr '\n' ',' < /run/s2-auth-backend | sed 's/,$//;s/ /_/g')"
    printf 'marker_sha256=%s\n' "$(sha256sum /run/s2/marker | cut -d' ' -f1)"
    printf 'ca_sha256=%s\n' "$(sha256sum /audit/ca.pem | cut -d' ' -f1)"
    while read -r name client daemon first last level cert; do
        digest=-
        if [ "$cert" != none ]; then
            digest=$(sha256sum "/run/s2/$cert.pem" | cut -d' ' -f1)
        fi
        printf 'listener name=%s client=%s daemon=%s passive=%s:%s tls=%s cert=%s cert_sha256=%s\n' \
            "$name" "$client" "$daemon" "$first" "$last" "$level" "$cert" "$digest"
    done < /run/s2/listeners
} > /audit/fixture.tmp
chmod 0644 /audit/fixture.tmp
: > /audit/control.jsonl
chmod 0644 /audit/control.jsonl
rm -f /run/s2-password /run/s2/marker
# Readiness is the manifest's existence, so it is published by one rename as the
# very last step: a reader never sees a partial manifest, and never sees one
# before the census was reset and the bootstrap secrets were removed.
mv /audit/fixture.tmp /audit/fixture.txt
# The taps and the daemons are children of this process, and a child that dies
# stays a zombie until someone reaps it, so liveness is read from its own task
# state, not from the existence of its pid. A fixture that quietly lost one of its
# six faces would turn every later refusal into a claim about a server that was
# never there.
alive() {
    [ -r "/proc/$1/status" ] || return 1
    ! grep -q '^State:[[:space:]]*Z' "/proc/$1/status"
}
while :; do
    for pid in $PIDS; do
        if ! alive "$pid"; then
            echo "fixture listener $pid exited" >&2
            exit 1
        fi
    done
    sleep 5
done
