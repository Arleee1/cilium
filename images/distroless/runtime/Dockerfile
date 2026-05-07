# Copyright Authors of Cilium
# SPDX-License-Identifier: Apache-2.0

ARG DISTRO=mariner
# For FIPS build in 1.26 with --build-arg GOEXPERIMENT=ms_nocgo_opensslcrypto
# Should be phased out in 1.27+ when CGO_ENABLED dynamically sets openssl cgo usage
ARG GOEXPERIMENT=""

ARG TESTER_IMAGE=quay.io/cilium/image-tester:dd09c8d3ef349a909fbcdc99279516baef153f22@sha256:c056d064cb47c97acd607343db5457e1d49d9338d6d8a87e93e23cc93f052c73
ARG GOLANG_IMAGE=mcr.microsoft.com/oss/go/microsoft/golang:1.26.1-1@sha256:df6482f65c8186602a54343b27e17fcb22c6b9ea2d8a6036ccf17b609294176e
ARG UBUNTU_IMAGE=docker.io/library/ubuntu:22.04@sha256:0e5e4a57c2499249aafc3b40fcd541e9a456aab7296681a3994d631587203f97
ARG MARINER_IMAGE=mcr.microsoft.com/azurelinux/base/core:3.0@sha256:c09a4e011a092a45b5c46ac5633253eb1e1106df028912b89cbe225d9061ef0b

ARG CILIUM_LLVM_IMAGE=quay.io/cilium/cilium-llvm:19.1.7-1775844888-ebe5854@sha256:745aef32b65911d14a8557014d0ae33c6ac0ed975b008e4721a3adf3ebda009a
ARG CILIUM_BPFTOOL_IMAGE=quay.io/cilium/cilium-bpftool:0db3a73729ceb42e947d826bb96a655be79e5317@sha256:de23c9546c4eafab33f75d6f5d129947bbbafc132dbd113c0cecc9a61929e6b0
ARG CILIUM_IPTABLES_IMAGE=quay.io/cilium/iptables:67f517af50e18f64cd12625021f1c39246bb4f92@sha256:d075f03e89aacf51908346ec8ed5d251b8d3ad528ce30a710fcd074cdf91f11d

FROM ${CILIUM_LLVM_IMAGE} AS llvm-dist
FROM ${CILIUM_BPFTOOL_IMAGE} AS bpftool-dist
FROM ${CILIUM_IPTABLES_IMAGE} AS iptables-dist

FROM ${GOLANG_IMAGE} AS gops-cni-builder
ARG GOEXPERIMENT
ENV GOEXPERIMENT=${GOEXPERIMENT}

WORKDIR /go/src/github.com/cilium/cilium/images/runtime
RUN --mount=type=bind,readwrite,target=/go/src/github.com/cilium/cilium/images/runtime --mount=target=/root/.cache,type=cache --mount=target=/go/pkg/mod,type=cache \
    images/runtime/build-gops.sh

ARG TARGETARCH
RUN --mount=type=bind,readwrite,target=/go/src/github.com/cilium/cilium/images/runtime --mount=target=/root/.cache,type=cache --mount=target=/go/pkg/mod,type=cache \
    images/runtime/download-cni.sh

# We will not build ubuntu based images into our manifest for size sake.
# Leaving this here for developer builds
FROM ${UBUNTU_IMAGE} AS ubuntu-fs

# Change the number to force the generation of a new git-tree SHA. Useful when
# we want to re-run 'apt-get upgrade' for stale images.
ENV FORCE_BUILD=4

# Update ubuntu packages to the most recent versions
RUN apt-get update && \
    apt-get upgrade -y && \
    apt-get install -y jq

WORKDIR /go/src/github.com/cilium/cilium/images/runtime
RUN --mount=type=bind,readwrite,target=/go/src/github.com/cilium/cilium/images/runtime \
    images/runtime/install-runtime-deps.sh

COPY --from=iptables-dist /iptables /iptables
RUN dpkg -i /iptables/*\.deb && rm -rf /iptables

FROM ${MARINER_IMAGE} AS mariner-fs

# Bootstrap CA certificates: the base image has no CA certs so tdnf cannot
# verify SSL when reaching the package repos. Temporarily disable sslverify
# in repo configs, install ca-certificates, then re-enable.
RUN sed -i 's/sslverify=1/sslverify=0/g' /etc/yum.repos.d/*.repo 2>/dev/null || true && \
    for repo in /etc/yum.repos.d/*.repo; do \
      if ! grep -q 'sslverify' "$repo"; then \
        sed -i '/^\[/a sslverify=0' "$repo"; \
      fi; \
    done && \
    tdnf install -y --nogpgcheck ca-certificates && \
    sed -i '/^sslverify=0$/d' /etc/yum.repos.d/*.repo 2>/dev/null || true && \
    sed -i 's/sslverify=0/sslverify=1/g' /etc/yum.repos.d/*.repo 2>/dev/null || true

WORKDIR /go/src/github.com/cilium/cilium/images/runtime
RUN --mount=type=bind,readwrite,target=/go/src/github.com/cilium/cilium/images/runtime \
    images/runtime/install-runtime-deps.sh mariner

FROM ${DISTRO}-fs AS intermediate

RUN --mount=type=bind,readwrite,target=/go/src/github.com/cilium/cilium/images/runtime \
    images/runtime/iptables-wrapper-installer.sh --no-sanity-check

COPY --from=llvm-dist /usr/local/bin/clang /usr/local/bin/llc /usr/local/bin/
COPY --from=bpftool-dist /usr/local /usr/local

COPY --from=gops-cni-builder /out /

FROM ${TESTER_IMAGE} AS test
COPY --from=intermediate / /
COPY --from=llvm-dist /test /test
COPY --from=bpftool-dist /test /test/
RUN /test/bin/cst -C /test/llvm
RUN /test/bin/cst -C /test/bpftool

FROM scratch AS release
COPY --from=intermediate / /
