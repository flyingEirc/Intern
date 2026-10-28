# syntax=docker/dockerfile:1
#
# P4 restore acceptance image: the pinned MySQL server plus the pinned Percona
# XtraBackup tools in one container, so one worker can stream a real backup,
# extract and prepare it, and rehearse the prepared datadir with the matching
# mysqld.
#
# The MySQL image is the final base: mysqld needs its complete layout
# (/usr/share/mysql-8.4 error messages, /usr/lib64/mysql/plugin components) and
# copying only the binary would not start. XtraBackup is added from the pinned
# PXB image. Its ELF RPATH is $ORIGIN/../lib/private, so the complete private
# library directory is copied there. Every other NEEDED soname
# (libaio.so.1, libgcrypt.so.20, libgpg-error.so.0, libssl.so.3, libcrypto.so.3,
# libstdc++.so.6, libm/libgcc_s/libc/libresolv) already exists in the MySQL
# image; the only absent one is libprocps.so.8, copied with its versioned file.
#
# The RUN line fails the build when any of the three binaries cannot load on
# this exact base, so a wrong library set can never reach the harness.
#
# Build arguments are the pinned digests from internal/physical/compat.json.
# Both images are linux/amd64 only.
ARG MYSQL_IMAGE
ARG PXB_IMAGE

FROM ${PXB_IMAGE} AS xtrabackup

FROM ${MYSQL_IMAGE}

COPY --from=xtrabackup /usr/bin/xtrabackup /usr/bin/xtrabackup
COPY --from=xtrabackup /usr/bin/xbstream /usr/bin/xbstream
COPY --from=xtrabackup /usr/lib/private/ /usr/lib/private/
COPY --from=xtrabackup /usr/lib64/libprocps.so.8 /usr/lib64/libprocps.so.8
COPY --from=xtrabackup /usr/lib64/libprocps.so.8.0.3 /usr/lib64/libprocps.so.8.0.3

RUN mysqld --version \
 && xtrabackup --version \
 && xbstream --version
