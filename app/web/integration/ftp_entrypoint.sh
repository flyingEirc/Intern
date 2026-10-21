#!/bin/sh
set -eu
umask 077
chmod 0711 /data
for user in normal quota; do
    mkdir -p "/data/$user"
    chown 1000:1000 "/data/$user"
    chmod 0700 "/data/$user"
done
# Official pure-pw reads the password twice from stdin. The root-only input is
# injected by Testcontainers, never a build argument, command argument or log.
pure-pw useradd normal -f /data/users.txt -u 1000 -g 1000 -d /data/normal < /run/s1-password
pure-pw useradd quota -f /data/users.txt -u 1000 -g 1000 -d /data/quota -N 1 < /run/s1-quota-password
pure-pw mkdb /data/users.pdb -f /data/users.txt
chmod 0600 /data/users.txt /data/users.pdb
# pure-pw's official crypt backend self-tests and selects bcrypt at cost 10.
# Bind the actual generated algorithm without exposing hashes or their salts;
# refuse an unexpected optional backend or weaker fallback.
: > /run/s1-auth-backend
while IFS=: read -r account password_hash rest; do
    case "$account" in
        normal|quota) ;;
        *) echo "unexpected fixture account" >&2; exit 1 ;;
    esac
    case "$password_hash" in
        '$2a$10$'*) ;;
        *) echo "unexpected fixture authentication backend" >&2; exit 1 ;;
    esac
    test "${#password_hash}" -eq 60
    printf '%s bcrypt cost=10\n' "$account" >> /run/s1-auth-backend
done < /data/users.txt
unset password_hash rest
rm /run/s1-password /run/s1-quota-password
# Private Docker peers have no public PTR records; do not gate the FTP greeting
# on external reverse-DNS resolution.
exec pure-ftpd -H -S0.0.0.0,2121 -lpuredb:/data/users.pdb -E -A -j -u1000 -p50000:50020
