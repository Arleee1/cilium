#!/usr/bin/env bash

# Copyright Authors of Cilium
# SPDX-License-Identifier: Apache-2.0

set -o xtrace
set -o errexit
set -o pipefail
set -o nounset

ubuntuPackages=(
  # Bash completion for Cilium
  bash-completion
  # Additional misc runtime dependencies
  iproute2
  iptables
  ipset
  kmod
  ca-certificates
  libatomic1
)

marinerPackages=(
  # Mariner runtime deps
  # Specs - https://github.com/microsoft/azurelinux/tree/3.0/SPECS

  # min reqs,
  ## https://docs.cilium.io/en/stable/operations/system_requirements/
  ## https://docs.cilium.io/en/stable/reference-guides/bpf/resources/
  jq
  iproute
  iptables
  ipset
  kmod
  ca-certificates
)

if [ "${1:-}" == "mariner" ]; then
  # # Update mariner packages to the most recent versions
  tdnf check-update -y
  tdnf install -y "${marinerPackages[@]}"

  tdnf clean all
else
  export DEBIAN_FRONTEND=noninteractive
  apt-get update

  # tzdata is one of the dependencies and a timezone must be set
  # to avoid interactive prompt when it is being installed
  ln -fs /usr/share/zoneinfo/UTC /etc/localtime

  apt-get install -y --no-install-recommends "${ubuntuPackages[@]}"

  apt-get purge --auto-remove
  apt-get clean
  rm -rf /var/lib/apt/lists/*
fi
