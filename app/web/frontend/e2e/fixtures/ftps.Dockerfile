# FTPS fixture for the Web frontend end-to-end stack.
#
# It reuses the exact recipe app/web/integration/ftps.Dockerfile and
# ftp.Dockerfile use for the native acceptance fixtures: the same pinned Alpine
# digest, the same Pure-FTPd 1.0.54 tarball checked against its SHA-512, the same
# configure flags and the same runtime package pins. The one difference is that
# this fixture has no audit taps: it serves two real TLS listeners (explicit
# AUTH TLS and the IMPLICIT_TLS build) on the Compose network, which is all a
# repository probe needs. The daemons themselves are the same 1.0.54 builds.
FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS build
RUN apk add --no-cache build-base=0.5-r4 musl=1.2.6-r3 musl-dev=1.2.6-r3 openssl-dev=3.5.9-r0
WORKDIR /build
ADD https://download.pureftpd.org/pub/pure-ftpd/releases/pure-ftpd-1.0.54.tar.gz /build/pure-ftpd-1.0.54.tar.gz
RUN echo 'f4a6d3020ff36fd0ee4c419e8b5927d27f56dbe164d49809b586a97fe6184e0b181eb465bdf509623c443ca3a9ed2fff32bea281b23511f52d55a68d33fbbb9d  pure-ftpd-1.0.54.tar.gz' | sha512sum -c - \
    && tar xzf pure-ftpd-1.0.54.tar.gz \
    && cd pure-ftpd-1.0.54 \
    && ./configure --prefix=/usr --with-puredb --with-quotas --with-throttling --with-ftpwho --with-tls \
    && make -j2 \
    && make DESTDIR=/out install

# The same source compiled once more with IMPLICIT_TLS defined: Pure-FTPd's
# implicit-TLS listener starts the handshake before the greeting and pins the
# data protection level to private.
RUN cp -a pure-ftpd-1.0.54 pure-ftpd-implicit \
    && cd pure-ftpd-implicit \
    && make clean \
    && ./configure --prefix=/usr --with-puredb --with-quotas --with-throttling --with-ftpwho --with-tls CPPFLAGS=-DIMPLICIT_TLS \
    && make -j2 \
    && make DESTDIR=/implicit install

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
RUN apk add --no-cache musl=1.2.6-r3 libssl3=3.5.9-r0 libcrypto3=3.5.9-r0 \
    && mkdir -p /data /run/pure-ftpd /run/tls
COPY --from=build /out/usr /usr
COPY --from=build /implicit/usr/sbin/pure-ftpd /usr/local/sbin/pure-ftpd-implicit
COPY --from=build /build/pure-ftpd-1.0.54/COPYING /usr/share/licenses/pure-ftpd/COPYING
COPY ftps_entrypoint.sh /usr/local/bin/ftps-fixture
RUN chmod 0755 /usr/local/bin/ftps-fixture
EXPOSE 2121 990
ENTRYPOINT ["/usr/local/bin/ftps-fixture"]
