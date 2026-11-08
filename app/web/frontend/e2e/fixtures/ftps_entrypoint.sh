#!/bin/sh
# FTPS fixture entry point: one Pure-FTPd 1.0.54 account on an explicit-TLS
# listener (AUTH TLS on 2121) and on the IMPLICIT_TLS build (990). The account
# password is injected as a root-only file and never appears in an argument, an
# environment variable or a log. The TLS certificate is a combined PEM (leaf
# certificate followed by its private key) mounted read-only.
set -eu
umask 077

for required in /run/ftp-password /run/tls/ftps.pem; do
    if [ ! -s "$required" ]; then
        echo "missing fixture input: $required" >&2
        exit 1
    fi
done

mkdir -p /data/normal/backups
chown 1000:1000 /data/normal /data/normal/backups
chmod 0700 /data/normal
# pure-pw reads the password twice; the injected file carries it twice.
pure-pw useradd normal -f /data/users.txt -u 1000 -g 1000 -d /data/normal < /run/ftp-password
pure-pw mkdb /data/users.pdb -f /data/users.txt
chmod 0600 /data/users.txt /data/users.pdb
rm -f /run/ftp-password

# Both daemons bind the container address so passive data connections are
# reachable from the intern container on the same Compose network. -Y 3 requires
# TLS for authentication and for every data transfer, which is what the FTPS
# client contract verifies.
pure-ftpd -H -S0.0.0.0,2121 -lpuredb:/data/users.pdb -E -A -j -u1000 \
    -p50100:50119 -c16 -C16 -Y 3 --certfile /run/tls/ftps.pem &
explicit=$!
pure-ftpd-implicit -H -S0.0.0.0,990 -lpuredb:/data/users.pdb -E -A -j -u1000 \
    -p50600:50619 -c16 -C16 -Y 3 --certfile /run/tls/ftps.pem &
implicit=$!

i=0
while [ "$i" -lt 60 ]; do
    if nc -z 127.0.0.1 2121 && nc -z 127.0.0.1 990; then
        break
    fi
    i=$((i + 1))
    sleep 1
done
if [ "$i" -ge 60 ]; then
    echo "the FTPS fixture listeners did not come up within 60 seconds" >&2
    exit 1
fi

# PID 1 is this shell, so it must forward the container stop signal to both
# daemons instead of ignoring it (a plain `sleep` loop made `docker stop` wait
# for its 120 s timeout).
trap 'kill "$explicit" "$implicit" 2>/dev/null; exit 0' TERM INT
while :; do
    kill -0 "$explicit" 2>/dev/null || { echo "explicit listener exited" >&2; exit 1; }
    kill -0 "$implicit" 2>/dev/null || { echo "implicit listener exited" >&2; exit 1; }
    sleep 5 &
    wait $! || true
done
