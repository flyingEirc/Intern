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
# Temporary comparison of the same credential with and without sodium_init.
auth_status=0
/usr/local/bin/s1-auth-diagnostic < /run/s1-password > /run/s1-auth-diagnostic || auth_status=$?
printf 'S1 auth diagnostic exit=%s\n' "$auth_status" >> /run/s1-auth-diagnostic
rm /run/s1-password /run/s1-quota-password
# Private Docker peers have no public PTR records; do not gate the FTP greeting
# on external reverse-DNS resolution.
exec pure-ftpd -H -S0.0.0.0,2121 -lpuredb:/data/users.pdb -E -A -j -u1000 -p50000:50020
