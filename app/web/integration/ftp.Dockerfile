FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6 AS build
# Use the unmodified system crypt backend. PureFTP 1.0.54 initializes optional
# libsodium in pure-pw but not the daemon, mismatching x86 password calibration.
RUN apk add --no-cache build-base=0.5-r4 musl=1.2.6-r2 musl-dev=1.2.6-r2 openssl-dev=3.5.9-r0
WORKDIR /build
ADD https://download.pureftpd.org/pub/pure-ftpd/releases/pure-ftpd-1.0.54.tar.gz /build/pure-ftpd-1.0.54.tar.gz
RUN echo 'f4a6d3020ff36fd0ee4c419e8b5927d27f56dbe164d49809b586a97fe6184e0b181eb465bdf509623c443ca3a9ed2fff32bea281b23511f52d55a68d33fbbb9d  pure-ftpd-1.0.54.tar.gz' | sha512sum -c - \
    && tar xzf pure-ftpd-1.0.54.tar.gz \
    && cd pure-ftpd-1.0.54 \
    && ./configure --prefix=/usr --with-puredb --with-quotas --with-throttling --with-ftpwho --with-tls \
    && make -j2 \
    && make DESTDIR=/out install

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
RUN apk add --no-cache musl=1.2.6-r2 libssl3=3.5.9-r0 libcrypto3=3.5.9-r0 \
    && mkdir -p /data /run/pure-ftpd \
    && chmod 0711 /data
COPY --from=build /out/usr /usr
COPY --from=build /build/pure-ftpd-1.0.54/COPYING /usr/share/licenses/pure-ftpd/COPYING
COPY ftp_entrypoint.sh /usr/local/bin/ftp-fixture
RUN chmod 0755 /usr/local/bin/ftp-fixture
ENTRYPOINT ["/usr/local/bin/ftp-fixture"]
