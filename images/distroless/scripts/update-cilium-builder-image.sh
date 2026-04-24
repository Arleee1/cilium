#!/usr/bin/env bash

# Copyright Authors of Cilium
# SPDX-License-Identifier: Apache-2.0

set -o xtrace
set -o errexit
set -o pipefail
set -o nounset

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")"/../../scripts && pwd)"

root_dir="$(git rev-parse --show-toplevel)"

cd "${root_dir}"

image="acnpublic.azurecr.io/cilium/cilium-builder-distroless"

image_tag="$(WITHOUT_SUFFIX=1 "${script_dir}/make-image-tag.sh" images/distroless/runtime)"

image_full="${image}:${image_tag}"

"${script_dir}/../distroless/builder/update-cilium-builder-image.sh" "${image_full}"
