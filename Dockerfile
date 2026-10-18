ARG GO_IMAGE=golang:1.27.0@sha256:4013ae0f9e7994f8535c58c811f8f863fbed38b72e0d51e6592156f758d66146
ARG MYSQL_IMAGE=mysql:9.7.1@sha256:cbade841779f1661300e705721f9d2ff159865cc7a8a291affbff43ac6ec7f1d

FROM ${GO_IMAGE} AS builder
WORKDIR /src
COPY go.work go.work.sum ./
COPY internal/go.mod internal/go.sum ./internal/
COPY app/cli/go.mod app/cli/go.sum ./app/cli/
COPY app/web/go.mod app/web/go.sum ./app/web/
RUN cd app/cli && go mod download
COPY internal ./internal
COPY app/cli ./app/cli
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false -o /out/intern ./app/cli

# The official server-minimal image lacks mysqlbinlog. Extract its unmodified
# official client RPM rather than silently using a different client version.
FROM ${MYSQL_IMAGE} AS client-tools
USER 0
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ARG CLIENT_RPM_URL
ARG CLIENT_RPM_SHA256
RUN microdnf install -y cpio-2.13-16.el9 && microdnf clean all
RUN test -n "${CLIENT_RPM_URL}" && test -n "${CLIENT_RPM_SHA256}" \
    && curl --fail --location --proto '=https' --tlsv1.2 "${CLIENT_RPM_URL}" -o /tmp/client.rpm \
    && echo "${CLIENT_RPM_SHA256}  /tmp/client.rpm" | sha256sum -c - \
    && rpm --import /etc/pki/rpm-gpg/RPM-GPG-KEY-mysql \
    && rpm --checksig /tmp/client.rpm \
    && mkdir /extract && cd /extract \
    && rpm2cpio /tmp/client.rpm | cpio -idmu

FROM ${MYSQL_IMAGE} AS runtime
USER 0
COPY --from=client-tools /extract/usr/bin/mysqlbinlog /usr/local/bin/mysqlbinlog
COPY --from=client-tools /extract/usr/share/doc/mysql-community-client /usr/share/licenses/intern/mysql-community-client
COPY --from=builder /out/intern /usr/local/bin/intern
COPY THIRD_PARTY_NOTICES /usr/share/licenses/intern/NOTICE
RUN mysql --version && mysqldump --version && mysqlbinlog --version \
    && install -d -m 0700 -o 10001 -g 10001 /home/intern /work/backups /work/state /run/secrets
ARG SOURCE_SHA
ARG MYSQL_IMAGE
LABEL org.opencontainers.image.title="Intern" \
      org.opencontainers.image.revision="${SOURCE_SHA}" \
      org.opencontainers.image.base.name="${MYSQL_IMAGE}"
ENV HOME=/home/intern XDG_CONFIG_HOME=/home/intern/.config
WORKDIR /work
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/intern"]
CMD ["--help"]
