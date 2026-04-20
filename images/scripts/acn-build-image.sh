#!/usr/bin/env bash

# Copyright Authors of Cilium
# SPDX-License-Identifier: Apache-2.0

# Build script that extends the same interface as build-image.sh with support
# for forwarding arbitrary docker build arguments.
#
# Positional arguments (same as build-image.sh):
#   $1           image name       (e.g. cilium-dev)
#   $2           image directory  (e.g. images/cilium)
#   $3           platforms        (e.g. linux/amd64,linux/arm64)
#   $4           output           (e.g. "type=image")
#   $5           builder          (buildx builder name)
#   $6..         registries       (one or more registry prefixes)
#
# Environment variables:
#   DOCKER_BUILD_ARGS  Space-separated KEY=VALUE pairs forwarded as
#                      --build-arg flags to docker buildx build.
#   ROOT_CONTEXT       If "true", use the repository root as the build context.
#   TEST               If "true", run the test target before building.
#   FORCE              If "true", skip registry checks and always build.
#   GOEXPERIMENT       Optional Go experiment flag (forwarded as --build-arg).
#   DOCKER_BUILD_TARGET  The Dockerfile stage to build.  Defaults to "release".
#                        Set to empty string to build the final (unnamed) stage
#                        (e.g. runtime/Dockerfile whose last layer is FROM scratch).

set -o errexit
set -o pipefail
set -o nounset

MAKER_IMAGE="${MAKER_IMAGE:-quay.io/cilium/image-maker:7de7f1c855ce063bdbe57fdfb28599a3ad5ec8f1@sha256:dde8500cbfbb6c41433d376fdfcb3831e2df9cec50cf4f49e8553dc6eba74e72}"

with_root_context="${ROOT_CONTEXT:-false}"

if [ "$#" -lt 6 ] ; then
  echo "$0 requires at least 6 arguments"
  exit 1
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

root_dir="$(git rev-parse --show-toplevel)"

cd "${root_dir}"

image_name="${1}"
image_dir="${2}"

platform="${3}"
output="${4}"
builder="${5}"

shift 5

registries=("${@}")

if [ -n "${IMAGE_TAG:-}" ] ; then
  image_tag="${IMAGE_TAG}"
elif [ "${with_root_context}" = "false" ] ; then
  image_tag="$("${script_dir}/make-image-tag.sh" "${image_dir}")"
else
  image_tag="$("${script_dir}/make-image-tag.sh")"
fi

tag_args=()
for registry in "${registries[@]}" ; do
  tag_args+=(--tag "${registry}/${image_name}:${image_tag}")
done

check_image_tag() {
  if [ -n "${MAKER_CONTAINER+x}" ] ; then
    crane digest "${1}" || (echo "error: crane returned $?" ; return 1)
  else
    # unlike with other utility scripts we don't want to self-re-exec inside the container, as native `docker buildx` is preferred
    docker run --env DOCKER_HUB_PUBLIC_ACCESS_ONLY=true --env QUAY_PUBLIC_ACCESS_ONLY=true --rm --volume "${root_dir}:/src" --workdir /src "${MAKER_IMAGE}" crane digest "${1}" || (echo "error: crane returned $?" ; return 1)
  fi
}

check_registries() {
  for registry in "${registries[@]}" ; do
    i="${registry}/${image_name}:${image_tag}"
    if ! check_image_tag "${i}" ; then
      echo "${i} doesn't exist"
      return 1
    fi
  done
}

do_build="${FORCE:-false}"

if [ "${do_build}" = "true" ] ; then
  echo "will force-build ${image_name}:${image_tag} without checking the registries"
fi

if [ "${do_build}" = "false" ] ; then
  case "${image_tag}" in
    *-dev)
      echo "will build ${image_name}:${image_tag} as it has dev suffix"
      do_build="true"
      ;;
    *-wip)
      echo "will build ${image_name}:${image_tag} as it has wip suffix"
      do_build="true"
      ;;
    *)
      if check_registries ; then
        echo "image ${image_name}:${image_tag} is already present in all of the registries"
        exit 0
      else
        echo "will build ${image_name}:${image_tag} as it's either a new version or not present in all of the registries"
        do_build="true"
      fi
      ;;
  esac
fi

do_test="${TEST:-false}"

build_target="${DOCKER_BUILD_TARGET-release}"

run_buildx() {
  build_args=(
    "--platform=${platform}"
    "--builder=${builder}"
    "--file=${image_dir}/Dockerfile"
  )
  if [ -n "${build_target}" ] ; then
    build_args+=("--target=${build_target}")
  fi
  if [ -n "${GOEXPERIMENT:-}" ] ; then
    build_args+=(--build-arg "GOEXPERIMENT=${GOEXPERIMENT}")
  fi
  # Forward each KEY=VALUE pair from DOCKER_BUILD_ARGS as a --build-arg.
  if [ -n "${DOCKER_BUILD_ARGS:-}" ] ; then
    read -ra _extra_args <<< "${DOCKER_BUILD_ARGS}"
    for arg in "${_extra_args[@]}" ; do
      build_args+=(--build-arg "${arg}")
    done
  fi
  if [ "${with_root_context}" = "false" ] ; then
    build_args+=("${image_dir}")
  else
    build_args+=("${root_dir}")
  fi
  if [ "${do_test}" = "true" ] ; then
    if ! docker buildx build --target=test "${build_args[@]}" ; then
      exit 1
    fi
  fi
  # For the final build, only add --target if build_target is set (the test
  # invocation above always uses --target=test regardless of build_target).
  docker buildx build --output="${output}" "${tag_args[@]}" "${build_args[@]}"
}

if [ "${do_build}" = "true" ] ; then
  echo "building ${image_name}:${image_tag}"
  if [ -n "${DOCKER_BUILD_ARGS:-}" ] ; then
    echo "with additional build args: ${DOCKER_BUILD_ARGS}"
  fi
  set -o xtrace
  if ! run_buildx ; then
    if [ -n "${DEBUG+x}" ] ; then
      buildkitd_container="$(docker ps --filter "ancestor=moby/buildkit:buildx-stable-1" --filter "name=${builder}" --format "{{.ID}}")"
      docker logs "${buildkitd_container}"
    fi
    exit 1
  fi
fi
