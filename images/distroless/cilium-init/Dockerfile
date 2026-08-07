# Copyright Authors of Cilium
# SPDX-License-Identifier: Apache-2.0

ARG CILIUM_BUILDER_IMAGE=acnpublic.azurecr.io/cilium/cilium-builder-distroless:fa5ef9bae30b897842f38cfa3978df794287d1a6
ARG CILIUM_RUNTIME_IMAGE=acnpublic.azurecr.io/cilium/cilium-runtime-distroless:fa5ef9bae30b897842f38cfa3978df794287d1a6
ARG NSENTER_BUILDER_IMAGE=mcr.microsoft.com/azurelinux/base/core:3.0
ARG MARINER_DISTROLESS=mcr.microsoft.com/azurelinux/distroless/base:3.0
ARG BUSYBOX_IMAGE=mcr.microsoft.com/azurelinux/busybox:1.36.1-azl3.0.20260728@sha256:1b421bbbea6c0a91c739cf46a70fd22e47fb84ac9b5eddf1650c0bbb51ce57b1

FROM ${CILIUM_RUNTIME_IMAGE} AS runtime

FROM --platform=${BUILDPLATFORM} ${CILIUM_BUILDER_IMAGE} AS builder
    # GOEXPERIMENT inherited from builder
    # For FIPS build in 1.26 with --build-arg GOEXPERIMENT=ms_nocgo_opensslcrypto
    # Should be phased out in 1.27+ when CGO_ENABLED dynamically sets openssl cgo usage

    # TARGETOS is an automatic platform ARG enabled by Docker BuildKit.
    ARG TARGETOS
    # TARGETARCH is an automatic platform ARG enabled by Docker BuildKit.
    ARG TARGETARCH
    # MODIFIERS are extra arguments to be passed to make at build time.
    ARG MODIFIERS

    WORKDIR /go/src/github.com/cilium/cilium
    # We must override NOSTRIP=1 to ensure binaries include debug symbols for extraction. They will be stripped subsequently
    # in accordance with the supplied/default NOSTRIP setting. See "Extract debug symbols" below.
    # Override SUBDIRS_CILIUM_CONTAINER to build only the necessary components for init-containers
    RUN --mount=type=bind,readwrite,target=/go/src/github.com/cilium/cilium \
        --mount=type=cache,target=/root/.cache \
        --mount=type=cache,target=/go/pkg \
        make GOARCH=${TARGETARCH} DESTDIR=/tmp/install/${TARGETOS}/${TARGETARCH} PKG_BUILD=1 $(echo $MODIFIERS | tr -d '"') NOSTRIP=1 \
        SUBDIRS_CILIUM_CONTAINER="cilium-dbg tools/mount tools/sysctlfix plugins/cilium-cni" \
        build-container install-container-binary

    # Extract debug symbols to /tmp/debug and strip the binaries if NOSTRIP is not set.
    # Use the appropriate objcopy for the target architecture.
    RUN set -xe && \
        export D=/tmp/debug/${TARGETOS}/${TARGETARCH} && \
        cd /tmp/install/${TARGETOS}/${TARGETARCH} && \
        find . -type f \
        -executable \
        -exec sh -c \
            'OBJCOPY_CMD=objcopy; \
            if [ "${TARGETARCH}" = "amd64" ]; then OBJCOPY_CMD=x86_64-linux-gnu-objcopy; \
            elif [ "${TARGETARCH}" = "arm64" ]; then OBJCOPY_CMD=aarch64-linux-gnu-objcopy; \
            fi; \
            filename=$(basename ${0}) && \
            ${OBJCOPY_CMD} --only-keep-debug ${0} ${0}.debug && \
            if ! echo "$MODIFIERS" | grep "NOSTRIP=1" ; then ${OBJCOPY_CMD} --strip-all ${0} && (cd $(dirname ${0}) && ${OBJCOPY_CMD} --add-gnu-debuglink=${filename}.debug ${filename}) ; fi && \
            mkdir -p $(dirname ${D}/${0}) && \
            mv -v ${0}.debug ${D}/${0}.debug' \
        {} \;

    # Check debug symbols are present
    RUN for f in $(find /tmp/debug -type f -name '*.debug') ; do readelf -S ${f} | grep -q \\.symtab || \
        (echo Debug symbols are missing in ${f} - possibly due to incorrect build parameters && false); done

    COPY images/cilium/init-container.sh \
        plugins/cilium-cni/install-plugin.sh \
        plugins/cilium-cni/cni-uninstall.sh \
        /tmp/install/${TARGETOS}/${TARGETARCH}

FROM ${NSENTER_BUILDER_IMAGE} AS nsenter
# Builds a statically linked nsenter binary
    RUN tdnf install -y \
        coreutils \
        git \
        make \
        cmake \
        ccache \
        binutils \
        ca-certificates \
        autoconf \
        automake \
        libtool \
        bison \
        gettext \
        flex \
        gawk \
        tar \
        gcc \
        kernel-headers \
        glibc-static

    RUN git clone --depth 1 https://github.com/util-linux/util-linux.git /src/util-linux
    WORKDIR /src/util-linux

    RUN ./autogen.sh && \
        ./configure \
        --disable-all-programs \
        --enable-nsenter \
        --enable-static \
        --without-ncurses \
        --without-readline

    # Build only nsenter, forcing all-static at make time so libtool
    # does not silently downgrade -static to -static-libtool-libs.
    RUN make nsenter LDFLAGS="-all-static"

# Intermediate stage for FIPS.
FROM ${MARINER_DISTROLESS} AS fips

FROM ${BUSYBOX_IMAGE} AS release
    # TARGETOS is an automatic platform ARG enabled by Docker BuildKit.
    ARG TARGETOS
    # TARGETARCH is an automatic platform ARG enabled by Docker BuildKit.
    ARG TARGETARCH

    COPY --from=nsenter /src/util-linux/nsenter /usr/bin/nsenter
    # init container script and CNI plugin installer
    COPY --from=builder /tmp/install/${TARGETOS}/${TARGETARCH} /

    # copy required fips libs directly from AZL 3.0 distroless base image for future proofing
    # Stage in /tmp/fips first, then merge with cp -r --update=none to avoid overwriting existing files.
    COPY --from=fips /etc /tmp/fips/etc
    COPY --from=fips /usr/lib /tmp/fips/usr/lib
    COPY --from=fips /usr/include /tmp/fips/usr/include
    RUN cp -rn /tmp/fips/etc/. /etc/ && \
        cp -rn /tmp/fips/usr/lib/. /usr/lib/ && \
        cp -rn /tmp/fips/usr/include/. /usr/include/ && \
        rm -rf /tmp/fips
    # strip out go conversion libs, saves 8MB
    RUN rm -rf /usr/lib/gconv

    ## CNI Loopback
    COPY --from=runtime /cni/loopback /cni/loopback

    ## iptables
    COPY --from=runtime /usr/sbin/*tables* /usr/sbin/
    # iptables libs
    COPY --from=runtime /usr/lib/libxtables* /usr/lib/libmnl* /usr/lib/libnft* /usr/lib/libc.* /usr/lib/
    COPY --from=runtime /usr/lib/iptables /usr/lib/iptables
    COPY --from=runtime /lib64/ld-linux* /lib64/
    ## alternatives, to point to correct iptables binary
    COPY --from=runtime /usr/sbin/alternatives /usr/sbin/alternatives
    COPY --from=runtime /var/lib/alternatives /var/lib/alternatives
