# syntax=docker/dockerfile:1
#
# Intern Web deployment image: the Web control plane plus the pinned physical
# backup tools in one image, so a single container can run the real Web API,
# execute xtrabackup against a mounted datadir, and rehearse a prepared datadir
# with the matching mysqld.
#
# Every base image is referenced by digest. The default digests are the pinned
# candidates: the Go toolchain is the same digest the CLI image builds with, and
# the MySQL and Percona XtraBackup digests are the two linux/amd64 entries of
# internal/physical/compat.json. app/web/deploy/compose.yaml passes the same
# values as build arguments, and the D1 acceptance test recomputes both pins from
# compat.json, so the image can never silently drift from the verified matrix.
#
# The runtime layer is the combination proven by app/web/integration/restore.Dockerfile:
# mysqld needs its complete layout (/usr/share/mysql-8.4 error messages,
# /usr/lib64/mysql/plugin components), so the MySQL image is the base and
# XtraBackup is added from the pinned PXB image. XtraBackup's ELF RPATH is
# $ORIGIN/../lib/private, so the whole private library directory is copied, and
# libprocps.so.8 is the only NEEDED soname the MySQL image lacks.
#
# Nothing is downloaded at run time and the runtime stage installs no packages:
# the only build-time network use is `go mod download` inside the builder stage,
# which the final image does not contain. The final RUN line fails the build when
# any of the three binaries cannot load on this exact base.
#
# The image defaults to UID/GID 999, the mysql user of the official MySQL image,
# which is also the owner of a MySQL datadir on a shared volume. Compose may
# override `user:`. ENTRYPOINT never goes through a shell.
#
# GPL materials: the image carries the Intern notice, the license text of each
# redistributed component as shipped by its own pinned image, the exact versions
# and base digests, and the source-provision statement. CI builds private
# candidates only; nothing here publishes the image or completes a GPL source
# offer for external distribution.
ARG GO_IMAGE=golang:1.27.0@sha256:4013ae0f9e7994f8535c58c811f8f863fbed38b72e0d51e6592156f758d66146
ARG MYSQL_IMAGE=mysql:8.4.11@sha256:85b9bf2e29cf836ecb8c2a15a935d4ba0c606631dff1dd79531a11983c638f2a
ARG PXB_IMAGE=percona/percona-xtrabackup:8.4.0-7@sha256:bf1478c188271b2478faddb92a27e8378364b672b8e399c0df1c17fef8cb0ceb

FROM ${GO_IMAGE} AS builder
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.work go.work.sum ./
COPY internal/go.mod internal/go.sum ./internal/
COPY app/cli/go.mod app/cli/go.sum ./app/cli/
COPY app/web/go.mod app/web/go.sum ./app/web/
RUN cd app/web && go mod download
COPY internal ./internal
COPY app/web ./app/web
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false -o /out/intern-web ./app/web

# License texts of the modules actually linked into intern-web. Each text is
# copied unmodified out of the module cache and the manifest records module path,
# version and file name, so the notice in the final image describes the binaries
# it ships. Only build-time module-cache reads; nothing is fetched at run time.
RUN set -eu; \
    mkdir -p /out/licenses/go; \
    : >/out/licenses/GO-MODULES.txt; \
    cd /src/app/web; \
    GOWORK=off go list -m -f '{{if .Dir}}{{.Path}} {{.Version}} {{.Dir}}{{end}}' all \
      | awk 'NF==3 && $2 != "(devel)" && $1 !~ /^github\.com\/flyingEirc\/Intern/' | LC_ALL=C sort > /tmp/modules.list; \
    listed=$(wc -l < /tmp/modules.list); \
    while read -r module version dir; do \
      [ -d "$dir" ] || { printf 'module %s has no module-cache directory\n' "$module" >&2; exit 1; }; \
      license=$(find "$dir" -maxdepth 1 -type f \( -iname 'LICENSE*' -o -iname 'COPYING*' \) | LC_ALL=C sort | head -n 1); \
      [ -n "$license" ] || { printf 'module %s %s ships no LICENSE or COPYING\n' "$module" "$version" >&2; exit 1; }; \
      safe=$(printf '%s' "$module" | tr '/@.+' '----'); \
      mkdir -p "/out/licenses/go/$safe"; \
      cp "$license" "/out/licenses/go/$safe/$(basename "$license")"; \
      printf '%s %s %s\n' "$module" "$version" "$(basename "$license")" >> /out/licenses/GO-MODULES.txt; \
    done < /tmp/modules.list; \
    test -s /out/licenses/GO-MODULES.txt; \
    [ "$listed" -eq "$(wc -l < /out/licenses/GO-MODULES.txt)" ] || { printf 'manifest lists %s of %s modules\n' "$(wc -l < /out/licenses/GO-MODULES.txt)" "$listed" >&2; exit 1; }; \
    chmod -R a+rX /out/licenses

FROM ${PXB_IMAGE} AS xtrabackup

FROM ${MYSQL_IMAGE} AS mysql-licenses

FROM ${MYSQL_IMAGE}

COPY --from=xtrabackup /usr/bin/xtrabackup /usr/bin/xtrabackup
COPY --from=xtrabackup /usr/bin/xbstream /usr/bin/xbstream
COPY --from=xtrabackup /usr/lib/private/ /usr/lib/private/
COPY --from=xtrabackup /usr/lib64/libprocps.so.8 /usr/lib64/libprocps.so.8
COPY --from=xtrabackup /usr/lib64/libprocps.so.8.0.3 /usr/lib64/libprocps.so.8.0.3
COPY --from=builder /out/intern-web /usr/local/bin/intern-web

# License materials for this image. Every text is copied unmodified from the
# pinned image that carries the matching binary, so the license always describes
# the exact build shipped here. The repository-wide THIRD_PARTY_NOTICES file
# describes the CLI verification runtime and is not the notice of this image.
COPY app/web/deploy/licenses/WEB-NOTICE.txt /usr/share/licenses/intern/NOTICE-WEB.txt
COPY app/web/deploy/licenses/GPL-SOURCE-OFFER.txt /usr/share/licenses/intern/GPL-SOURCE-OFFER.txt
COPY --from=mysql-licenses /usr/share/doc/mysql-community-server-minimal/LICENSE /usr/share/licenses/intern/mysql-community-server/LICENSE
COPY --from=xtrabackup /usr/share/doc/percona-xtrabackup-84/LICENSE /usr/share/licenses/intern/percona-xtrabackup/COPYING
# libprocps.so.8 is copied out of the pinned PXB image and belongs to no package
# in the final image, so its own license texts ship with it. The pinned image
# carries procps-ng's COPYING and the GNU Library General Public License v2 text
# (COPYING.LIB); upstream describes the package as LGPL-2.0-or-later /
# LGPL-2.1-or-later.
COPY --from=xtrabackup /usr/share/licenses/procps-ng/COPYING /usr/share/licenses/intern/procps-ng/COPYING
COPY --from=xtrabackup /usr/share/licenses/procps-ng/COPYING.LIB /usr/share/licenses/intern/procps-ng/COPYING.LIB
# The modules linked into intern-web, with their license texts and manifest.
COPY --from=builder /out/licenses /usr/share/licenses/intern/go-internals

RUN mysqld --version \
 && xtrabackup --version \
 && xbstream --version \
 && intern-web --help >/dev/null

ARG SOURCE_SHA=unknown
ARG GO_IMAGE
ARG MYSQL_IMAGE
ARG PXB_IMAGE
RUN set -eu \
 && mkdir -p /var/lib/intern/metadata /var/lib/intern/backups /var/lib/intern/work /var/lib/intern/export \
 && chown -R 999:999 /var/lib/intern \
 && chmod 0700 /var/lib/intern /var/lib/intern/metadata /var/lib/intern/backups /var/lib/intern/work \
 && chmod 0750 /var/lib/intern/export \
 && install -d -m 0755 /run/secrets \
 && { \
      echo "Intern Web deployment image: exact component provenance."; \
      echo "This file is generated at build time by app/web/deploy/Dockerfile."; \
      echo ""; \
      echo "intern_source_sha=${SOURCE_SHA}"; \
      echo "go_image=${GO_IMAGE}"; \
      echo "mysql_image=${MYSQL_IMAGE}"; \
      echo "pxb_image=${PXB_IMAGE}"; \
      echo "intern_web=$(/usr/local/bin/intern-web --help 2>&1 | head -n 1)"; \
      echo "mysqld=$(mysqld --version)"; \
      echo "xtrabackup=$(xtrabackup --version 2>&1)"; \
      echo "xbstream=$(xbstream --version 2>&1)"; \
      echo ""; \
      echo "License texts shipped with this image:"; \
      echo "  /usr/share/licenses/intern/NOTICE-WEB.txt"; \
      echo "  /usr/share/licenses/intern/GPL-SOURCE-OFFER.txt"; \
      echo "  /usr/share/licenses/intern/mysql-community-server/LICENSE"; \
      echo "  /usr/share/licenses/intern/percona-xtrabackup/COPYING"; \
      echo "  /usr/share/licenses/intern/procps-ng/COPYING and COPYING.LIB"; \
      echo "  /usr/share/licenses/intern/go-internals/GO-MODULES.txt (texts under go-internals/go/)"; \
      echo ""; \
      echo "Copied files that belong to no package of the final image (provenance and digest):"; \
      for path in $( { ls /usr/lib/private/*.so* 2>/dev/null || true; echo /usr/lib64/libprocps.so.8; echo /usr/lib64/libprocps.so.8.0.3; } | LC_ALL=C sort ); do \
        printf 'copied %s size=%s sha256=%s\n' "$path" "$(stat -c %s "$path")" "$(sha256sum "$path" | cut -d' ' -f1)"; \
      done; \
    } >/usr/share/licenses/intern/VERSIONS.txt \
 && test -s /usr/share/licenses/intern/go-internals/GO-MODULES.txt \
 && chmod 0644 /usr/share/licenses/intern/VERSIONS.txt

LABEL org.opencontainers.image.title="Intern Web" \
      org.opencontainers.image.description="Intern Web backup control plane with pinned MySQL 8.4 and Percona XtraBackup tools" \
      org.opencontainers.image.source="https://github.com/flyingEirc/Intern" \
      org.opencontainers.image.revision="${SOURCE_SHA}" \
      org.opencontainers.image.base.name="${MYSQL_IMAGE}" \
      io.github.flyingeirc.intern.mysql-image="${MYSQL_IMAGE}" \
      io.github.flyingeirc.intern.pxb-image="${PXB_IMAGE}" \
      io.github.flyingeirc.intern.go-image="${GO_IMAGE}" \
      io.github.flyingeirc.intern.licenses="/usr/share/licenses/intern"

USER 999:999
ENTRYPOINT ["/usr/local/bin/intern-web"]
CMD ["serve"]
