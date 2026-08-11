# Copyright Authors of Cilium
# SPDX-License-Identifier: Apache-2.0

ARG DISTRO=mariner

ARG CILIUM_BUILDER_IMAGE=acnpublic.azurecr.io/cilium/cilium-builder-distroless:fa5ef9bae30b897842f38cfa3978df794287d1a6
ARG CILIUM_RUNTIME_IMAGE=acnpublic.azurecr.io/cilium/cilium-runtime-distroless:fa5ef9bae30b897842f38cfa3978df794287d1a6

#
# Distroless
#
## distroless/base brings in required openssl libs.
ARG MARINER_DISTROLESS=mcr.microsoft.com/azurelinux/distroless/base:3.0
ARG UBUNTU_DISTROLESS=TODO

#
# cilium-envoy from github.com/cilium/proxy
#
ARG CILIUM_ENVOY_IMAGE=quay.io/cilium/cilium-envoy:v1.34.7-1757592137-1a52bb680a956879722f48c591a2ca90f7791324@sha256:7932d656b63f6f866b6732099d33355184322123cfe1182e6f05175a3bc2e0e0

FROM ${CILIUM_ENVOY_IMAGE} AS cilium-envoy

#
# Cilium incremental build. Should be fast given builder-deps is up-to-date!
#
# cilium-builder tag is the date on which the compatible build image
# was pushed.  If a new version of the build image is needed, it needs
# to be tagged with a new date and this file must be changed
# accordingly.  Keeping the old images available will allow older
# versions to be built while allowing the new versions to make changes
# that are not backwards compatible.
#
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

#
# Please do not add any dependency updates before the 'make install' here,
# as that will mess with caching for incremental builds!
#
WORKDIR /go/src/github.com/cilium/cilium
# We must override NOSTRIP=1 to ensure binaries include debug symbols for extraction. They will be stripped subsequently
# in accordance with the supplied/default NOSTRIP setting. See "Extract debug symbols" below.
RUN --mount=type=bind,readwrite,target=/go/src/github.com/cilium/cilium \
    --mount=type=cache,target=/root/.cache \
    --mount=type=cache,target=/go/pkg \
    make -C images -f Makefile.acn GOARCH=${TARGETARCH} DESTDIR=/tmp/install/${TARGETOS}/${TARGETARCH} PKG_BUILD=1 \
    $(echo $MODIFIERS | tr -d '"') NOSTRIP=1 acn-build-distroless-agent

RUN --mount=type=bind,readwrite,target=/go/src/github.com/cilium/cilium \
    --mount=type=cache,target=/root/.cache \
    --mount=type=cache,target=/go/pkg \
    # install-bash-completion will execute the bash_completion script. It is
    # fine to run this with same architecture as BUILDARCH since the output of
    # bash_completion is the same for both architectures.
    make GOARCH=${BUILDARCH} DESTDIR=/tmp/install/${TARGETOS}/${TARGETARCH} PKG_BUILD=1 $(echo $MODIFIERS | tr -d '"') \
    install-bash-completion licenses-all && \
    mv LICENSE.all /tmp/install/${TARGETOS}/${TARGETARCH}/LICENSE.all

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


#
# Cilium runtime install.
#
# cilium-runtime tag is a date on which the compatible runtime base
# was pushed.  If a new version of the runtime is needed, it needs to
# be tagged with a new date and this file must be changed accordingly.
# Keeping the old runtimes available will allow older versions to be
# built while allowing the new versions to make changes that are not
# backwards compatible.
#

FROM ${CILIUM_RUNTIME_IMAGE} AS ubuntu-fs
# Placeholder until distroless work is complete
FROM ubuntu-fs AS ubuntu-intermediate
## ubuntu-fs is not a distroless image currently. No operations needed until distroless as fs is correct already.
# TODO, leverage gcr.distroless as seen in other upstream dockerfiles to make ubuntu-fs distroless as well


#
# Mariner Distroless
# Main Changes can be found from runtime with
## tdnf repoquery --list <pkg>
## ldd <pkg> - To find required libs
## https://brave-ocean-0baeae310.5.azurestaticapps.net/#/packages (this is an internal site)
#
# Useful Tool/Links
## dive - https://github.com/wagoodman/dive
## system reqs - https://docs.cilium.io/en/stable/operations/system_requirements/
#
FROM ${CILIUM_RUNTIME_IMAGE} AS mariner-fs
# syslink bpftool
# RUN ln -s usr/sbin/bpftool usr/bin/bpftool

FROM ${MARINER_DISTROLESS} AS mariner-intermediate
## usr/lib
# Generic/system libs
COPY --from=mariner-fs /lib64/ld-linux* /lib64/
# Networking libs
COPY --from=mariner-fs \
  /usr/lib/libc.* /usr/lib/libmnl* /usr/lib/libelf* /usr/lib/libz* \
  /usr/lib/libcap* /usr/lib/libgcc* /usr/lib/libip* \
  /usr/lib/

## iproute2
COPY --from=mariner-fs /usr/sbin/ip /usr/sbin/genl usr/sbin/
# ip libs
COPY --from=mariner-fs /usr/lib/libselinux* /usr/lib/libbpf* /usr/lib/

## iptables
COPY --from=mariner-fs /usr/sbin/*tables* /usr/sbin/
# iptables libs
COPY --from=mariner-fs /usr/lib/libxtables* /usr/lib/iptables /usr/lib/libnft*  /usr/lib/
COPY --from=mariner-fs /usr/lib/iptables /usr/lib/iptables

## ipset
COPY --from=mariner-fs /usr/sbin/ipset* /usr/sbin/
# ipset libs
# captured in Networking Libs above

## kmod
# kmod is a collection of tools for managing kernel modules
# depmod, insmod, lsmod, modinfo, modprobe, rmmod
COPY --from=mariner-fs /usr/sbin/depmod /usr/sbin/insmod /usr/sbin/lsmod /usr/sbin/modinfo /usr/sbin/modprobe /usr/sbin/rmmod /usr/sbin/
# all alias to bin/kmod
COPY --from=mariner-fs /bin/kmod /bin/
# kmod libs
COPY --from=mariner-fs /usr/lib/liblzma* /usr/lib/

## llc(llvm), clang, bpftool
COPY --from=mariner-fs /usr/local/bin/clang /usr/local/bin/llc /usr/local/bin/
COPY --from=mariner-fs /usr/local /usr/local
# libs
COPY --from=mariner-fs /usr/lib/libstdc* /usr/lib/libm.* /usr/lib/

## system-wide configuration files
COPY --from=mariner-fs /etc/ /etc/

## CNI Loopback
COPY --from=mariner-fs /cni/loopback /cni/loopback

## gops
COPY --from=mariner-fs /bin/gops /bin/gops

## alternatives
COPY --from=mariner-fs /usr/sbin/alternatives /usr/sbin/alternatives
COPY --from=mariner-fs /var/lib/alternatives /var/lib/alternatives

## Cat & Uname
# Required for cilium CLI. Connectivity test will not run without it.
COPY --from=mariner-fs usr/bin/cat /usr/bin/cat
COPY --from=mariner-fs usr/bin/uname /usr/bin/uname

#
# Final Image
#
FROM ${DISTRO}-intermediate AS release
# TARGETOS is an automatic platform ARG enabled by Docker BuildKit.
ARG TARGETOS
# TARGETARCH is an automatic platform ARG enabled by Docker BuildKit.
ARG TARGETARCH
LABEL maintainer="maintainer@cilium.io"
COPY --from=cilium-envoy /usr/lib/libcilium.so /usr/lib/libcilium.so
COPY --from=cilium-envoy /usr/bin/cilium-envoy /usr/bin/cilium-envoy-starter /usr/bin/
# When used within the Cilium container, Hubble CLI should target the
# local unix domain socket instead of Hubble Relay.
ENV HUBBLE_SERVER=unix:///var/run/cilium/hubble.sock
COPY --from=builder /tmp/install/${TARGETOS}/${TARGETARCH} /
WORKDIR /home/cilium

ENV INITSYSTEM="SYSTEMD"
CMD ["/usr/bin/cilium-dbg"]

#
# Cilium debug image.
#
# Typical image bulids will stop above at the 'release' target, but
# developers follow this Dockerfile to the end. Starting from a release
# image, install delve debugger and wrap the cilium-agent binary calls
# with a script that automatically provisions the debugger on a
# dedicated port.
FROM release AS debug
# TARGETOS is an automatic platform ARG enabled by Docker BuildKit.
ARG TARGETOS
# TARGETARCH is an automatic platform ARG enabled by Docker BuildKit.
ARG TARGETARCH
ARG DEBUG_HOLD
ENV DEBUG_HOLD=${DEBUG_HOLD}
COPY --from=builder /tmp/install/${TARGETOS}/${TARGETARCH}/usr/bin/cilium-agent /usr/bin/cilium-agent-bin
COPY --from=builder /go/bin/dlv /usr/bin/dlv
COPY --from=builder /out/${TARGETOS}/${TARGETARCH}/bin/debug-wrapper /usr/bin/cilium-agent

# Copy in the debug symbols in case the binaries were stripped
COPY --from=builder /tmp/debug/${TARGETOS}/${TARGETARCH} /usr/lib/debug

# Ensure dlv finds the debug symbols. Due to CGO_ENABLED=0, we have no GNU build-id, so Delve's default search path
# is insufficient.
RUN mkdir -p ${HOME}/.config/dlv && \
    echo 'debug-info-directories: ["/usr/lib/debug/.build-id","/usr/lib/debug"]' > ${HOME}/.config/dlv/config.yml && \
    ln -s /usr/lib/debug/cilium-agent.debug /usr/lib/debug/cilium-agent-bin.debug
