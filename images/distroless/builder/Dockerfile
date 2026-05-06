# Copyright Authors of Cilium
# SPDX-License-Identifier: Apache-2.0

ARG DISTRO=mariner
# For FIPS build in 1.26 with --build-arg GOEXPERIMENT=ms_nocgo_opensslcrypto
# Should be phased out in 1.27+ when CGO_ENABLED dynamically sets openssl cgo usage
ARG GOEXPERIMENT=""

ARG COMPILERS_IMAGE=quay.io/cilium/image-compilers:5569a29cea6b3ad50aeb03102aaf3dc03841197c@sha256:b15dbedb7c49816c74a765e2f6ecdb9359763b8e4e4328d794f48b9cefae9804
ARG CILIUM_RUNTIME_IMAGE=acnpublic.azurecr.io/cilium/cilium-runtime-distroless:fa5ef9bae30b897842f38cfa3978df794287d1a6
ARG TESTER_IMAGE=quay.io/cilium/image-tester:dd09c8d3ef349a909fbcdc99279516baef153f22@sha256:c056d064cb47c97acd607343db5457e1d49d9338d6d8a87e93e23cc93f052c73
ARG GOLANG_IMAGE=mcr.microsoft.com/oss/go/microsoft/golang:1.26.1-1@sha256:df6482f65c8186602a54343b27e17fcb22c6b9ea2d8a6036ccf17b609294176e
ARG CILIUM_LLVM_IMAGE=quay.io/cilium/cilium-llvm:19.1.7-1775844888-ebe5854@sha256:745aef32b65911d14a8557014d0ae33c6ac0ed975b008e4721a3adf3ebda009a

FROM ${COMPILERS_IMAGE} AS compilers-image

FROM ${GOLANG_IMAGE} AS golang-dist

FROM ${CILIUM_LLVM_IMAGE} AS llvm-dist

FROM ${CILIUM_RUNTIME_IMAGE} AS ubuntu-fs

# Change the number to force the generation of a new git-tree SHA. Useful when
# we want to re-run 'apt-get upgrade' for stale images.
ENV FORCE_BUILD=1

RUN \
    apt-get update && \
    apt-get upgrade -y --no-install-recommends && \
    apt-get install -y --no-install-recommends \
      # Install cross tools for both arm64 on amd64
      gcc-aarch64-linux-gnu \
      g++-aarch64-linux-gnu \
      libc6-dev-arm64-cross \
      binutils-aarch64-linux-gnu \
      gcc-x86-64-linux-gnu \
      g++-x86-64-linux-gnu \
      libc6-dev-amd64-cross \
      binutils-x86-64-linux-gnu \
      # Dependencies to unzip protoc
      unzip \
      # Base Cilium-build dependencies
      binutils \
      coreutils \
      curl \
      gcc \
      git \
      libc6-dev \
      make \
      ccache && \
    apt-get purge --auto-remove && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

FROM ${CILIUM_RUNTIME_IMAGE} AS mariner-fs

# TARGETARCH is an automatic platform ARG enabled by Docker BuildKit.
ARG TARGETARCH
RUN tdnf install -y \
        coreutils \
        git \
        make \
        ccache \
        binutils \
        unzip \
        glibc-devel \
        gcc \
        kernel-headers && \
    if [ "${TARGETARCH}" = "amd64" ]; then \
        tdnf install -y binutils-aarch64-linux-gnu; \
    fi

RUN tdnf clean all && \
    rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

FROM ${DISTRO}-fs AS intermediate
ARG DISTRO
ARG GOEXPERIMENT
ENV GOEXPERIMENT=${GOEXPERIMENT}

COPY --from=compilers-image /usr/lib/aarch64-linux-gnu /usr/lib/aarch64-linux-gnu

COPY --from=golang-dist /usr/local/go /usr/local/go
RUN mkdir -p /go
ENV GOROOT /usr/local/go
ENV GOPATH /go
ENV PATH "${GOROOT}/bin:${GOPATH}/bin:${PATH}"

RUN GOARCH=${TARGETARCH} CGO_ENABLED=0 go install github.com/go-delve/delve/cmd/dlv@latest

WORKDIR /go/src/github.com/cilium/cilium/images/builder
RUN --mount=type=bind,readwrite,target=/go/src/github.com/cilium/cilium/images/builder \
    images/builder/install-gitconfig.sh

WORKDIR /go/src/github.com/cilium/cilium/images/builder
RUN --mount=type=bind,readwrite,target=/go/src/github.com/cilium/cilium/images/builder \
    --mount=type=cache,target=/root/.cache \
    --mount=type=cache,target=/go/pkg \
    images/builder/build-debug-wrapper.sh

WORKDIR /go/src/github.com/cilium/cilium/images/builder
RUN --mount=type=bind,readwrite,target=/go/src/github.com/cilium/cilium/images/builder \
    images/builder/install-protoc.sh

RUN --mount=type=bind,readwrite,target=/go/src/github.com/cilium/cilium/images/builder \
    images/distroless/builder/install-protoplugins.sh ${DISTRO}

# used to facilitate the verifier tests
COPY --from=llvm-dist /usr/local/bin/llvm-objcopy /bin/

# Create a cache directory with 777 so that we can run the builder image and
# compile golang code from any UID.
RUN mkdir -p /.cache && chmod 777 /.cache && \
    mkdir -p /go/bin && chmod 777 /go/bin

FROM ${TESTER_IMAGE} AS test
COPY --from=intermediate / /
COPY test /test
RUN /test/bin/cst

# this image is large, and re-using layers is beneficial,
# so final images is not squashed
FROM intermediate AS release
LABEL maintainer="maintainer@cilium.io"
WORKDIR /go/src/github.com/cilium/cilium
