#!/usr/bin/env python3
# Copyright Authors of Cilium
# SPDX-License-Identifier: Apache-2.0
"""Prepare narrow, concurrency-guarded patches from saved POC objects; never apply them."""

import argparse
import copy
import hashlib
import json
import os
import pathlib
import re


PARENT = "spiffe://spiffe.cilium/ns/cilium-spire/sa/spire-agent"
CLASS = "cilium-spire"
PREFIX = "cilium-scm-sa-poc-1108"
NEWER_FLAGS = (
    "--enable-ztunnel-spiffeid-set-crd=",
    "--mesh-auth-spire-spiffeid-set-spec=",
    "--mesh-auth-spire-registration-name-prefix=",
    "--mesh-auth-spire-registration-class=",
    "--mesh-auth-spire-registration-service-accounts=",
    "--mesh-auth-spire-registration-labels=",
    "--mesh-auth-spire-registration-annotations=",
)
INPUT_BLOCK = """clusterSPIFFEIDSet:
  allowedInputKinds:
    - apiVersion: "v1"
      kind: "Namespace"
    - apiVersion: "v1"
      kind: "ServiceAccount"
"""
STATIC_RULES = [
    {"apiGroups": ["spire.spiffe.io"], "resources": ["clusterstaticentries"], "verbs": ["get", "list", "watch"]},
    {"apiGroups": ["spire.spiffe.io"], "resources": ["clusterstaticentries/status"], "verbs": ["get", "patch", "update"]},
]


def require(condition, message):
    if not condition:
        raise ValueError(message)


def get_path(obj, path):
    value = obj
    for key in path.split("/")[1:]:
        key = key.replace("~1", "/").replace("~0", "~")
        value = value[int(key)] if isinstance(value, list) else value[key]
    return value


def guarded_patch(obj, changes):
    patch = [{"op": "test", "path": "/metadata/" + key, "value": obj["metadata"][key]}
             for key in ("uid", "resourceVersion")]
    for path, value in changes.items():
        old = get_path(obj, path)
        if old != value:
            patch.extend([{"op": "test", "path": path, "value": copy.deepcopy(old)},
                          {"op": "replace", "path": path, "value": copy.deepcopy(value)}])
    return patch


def operator_args(original):
    require("--enable-ztunnel-spiffeid-crd=true" in original, "Existing CRD mode is required")
    require("--mesh-auth-spiffe-trust-domain=spiffe.cilium" in original, "Preserve the live trust domain")
    result = [value for value in original if not value.startswith(NEWER_FLAGS)]
    for name, value in (("skip-crd-creation", "true"), ("mesh-auth-spire-crd-parent-id", PARENT),
                        ("mesh-auth-spire-crd-class", CLASS)):
        expected = "--" + name + "=" + value
        existing = [arg for arg in result if arg == "--" + name or arg.startswith("--" + name + "=")]
        require(not existing or existing == [expected], "Unexpected existing restoration override: " + name)
        if not existing:
            result.append(expected)
    return result


def producer_rules(original):
    rules = copy.deepcopy(original)
    matching = [rule for rule in rules if "spire.spiffe.io" in rule.get("apiGroups", [])]
    require(len(matching) == 1, "Expected exactly one current SPIRE producer permission")
    rule = matching[0]
    current = {"apiGroups": ["spire.spiffe.io"], "resources": ["clusterspiffeidsets", "clusterstaticentries"],
               "verbs": ["get", "list", "create", "update"]}
    restored = {"apiGroups": ["spire.spiffe.io"], "resources": ["clusterstaticentries"],
                "verbs": ["get", "list", "create", "update", "delete"]}
    require(rule in (current, restored), "Unexpected SPIRE producer grants")
    rule["resources"] = ["clusterstaticentries"]
    rule["verbs"] = ["get", "list", "create", "update", "delete"]
    return rules


def static_config(original):
    for line in ('trustDomain: "spiffe.cilium"', 'className: "cilium-spire"',
                 'entryIDPrefix: "' + PREFIX + '"', "watchClassless: false",
                 "  clusterSPIFFEIDs: false", "  clusterStaticEntries: true",
                 "  clusterFederatedTrustDomains: false", "  clusterSPIFFEIDSets: true"):
        require(original.splitlines().count(line) == 1, "Unexpected SCM configuration: " + line)
    require("clusterServiceAccountSPIFFEIDs" not in original and "entryIDPrefixCleanup" not in original,
            "Unexpected legacy source or prefix-cleanup setting")
    require(original.count(INPUT_BLOCK) == 1, "Unexpected generic input allowlist")
    return original.replace(INPUT_BLOCK, "").replace("  clusterSPIFFEIDSets: true\n", "  clusterSPIFFEIDSets: false\n")


def prepare(saved, image):
    require(re.fullmatch(r"localhost/scm-sa-poc/operator-generic:static-[0-9a-f]{12}", image),
            "Use the immutable local source-tagged operator image")
    deployment, role, config, scm_role, server = (
        saved[key] for key in ("deployment", "operator_role", "controller_config", "controller_role", "server"))
    for obj, kind, name, namespace in (
        (deployment, "Deployment", "cilium-operator", "kube-system"),
        (role, "ClusterRole", "cilium-operator", None),
        (config, "ConfigMap", "spire-server-crd-controller-manager", "cilium-spire"),
        (scm_role, "ClusterRole", "spire-server-controller-manager", None),
        (server, "StatefulSet", "spire-server-crd", "cilium-spire"),
    ):
        require(obj["kind"] == kind and obj["metadata"]["name"] == name and obj["metadata"].get("namespace") == namespace,
                "Unexpected saved POC object: " + name)
    containers = deployment["spec"]["template"]["spec"]["containers"]
    require(len(containers) == 1 and containers[0]["name"] == "cilium-operator", "Unexpected operator containers")
    require(deployment["spec"]["replicas"] == 2, "Preserve the two operator replicas")
    require(server["spec"]["replicas"] == 2, "Preserve the two independent SPIRE stores")
    args = operator_args(containers[0]["args"])
    producer = producer_rules(role["rules"])
    original_config = config["data"]["controller-manager.yaml"]
    final_config = static_config(original_config)
    expected_role = [
        {"apiGroups": ["spire.spiffe.io"], "resources": ["clusterspiffeidsets", "clusterstaticentries"],
         "verbs": ["get", "list", "watch"]},
        {"apiGroups": ["spire.spiffe.io"], "resources": ["clusterspiffeidsets/status", "clusterstaticentries/status"],
         "verbs": ["get", "patch", "update"]},
        {"apiGroups": [""], "resources": ["namespaces"], "verbs": ["get", "list", "watch"]},
        {"apiGroups": [""], "resources": ["serviceaccounts"], "verbs": ["get", "list", "watch"]},
    ]
    require(scm_role["rules"] == expected_role, "Unexpected current SCM grants")
    annotation = "cilium.io/spire-controller-manager-configmap-checksum"
    require(annotation in server["spec"]["template"]["metadata"]["annotations"], "Missing SCM rollout checksum")
    return {
        "overlap-operator-deployment.patch.json": guarded_patch(deployment, {
            "/spec/template/spec/containers/0/image": image,
            "/spec/template/spec/containers/0/args": args,
        }),
        "overlap-operator-role.patch.json": guarded_patch(role, {"/rules": producer}),
        "static-only-config.patch.json": guarded_patch(config, {"/data/controller-manager.yaml": final_config}),
        "static-only-role.patch.json": guarded_patch(scm_role, {"/rules": STATIC_RULES}),
        "static-only-server.patch.json": guarded_patch(server, {
            "/spec/template/metadata/annotations/" + annotation.replace("/", "~1"):
                hashlib.sha256(final_config.encode()).hexdigest(),
        }),
        "restore-summary.json": {
            "operator_image": image, "operator_args": args, "controller_config": final_config,
            "operator_spire_rule": next(rule for rule in producer if rule["apiGroups"] == ["spire.spiffe.io"]),
            "final_controller_rules": STATIC_RULES,
            "required_before_static_only": [
                "All six equivalent operator-owned CSEs exist and converge on both stores",
                "All ten original full registrations/IDs/revisions remain identical",
                "Recorded temporary proof rules are absent or safely withdrawn while Sets stay enabled",
                "Primary Set is withdrawn after CSE adoption",
                "No owned-prefix identityset. entry remains on either store",
                "No new identity/lifecycle cases ran during the overlap",
            ],
            "whole_chart_rollback": False, "applies_to_cluster": False,
        },
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", required=True, type=pathlib.Path)
    parser.add_argument("--output", required=True, type=pathlib.Path)
    parser.add_argument("--operator-image", required=True)
    args = parser.parse_args()
    files = {
        "deployment": "deployment-cilium-operator.json",
        "operator_role": "clusterrole-cilium-operator.json",
        "controller_config": "configmap-spire-server-crd-controller-manager.json",
        "controller_role": "clusterrole-spire-server-controller-manager.json",
        "server": "statefulset-spire-server-crd.json",
    }
    results = prepare({key: json.loads((args.baseline / name).read_text()) for key, name in files.items()}, args.operator_image)
    args.output.mkdir(mode=0o700, parents=True, exist_ok=False)
    for name, value in results.items():
        descriptor = os.open(args.output / name, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        with os.fdopen(descriptor, "w") as stream:
            json.dump(value, stream, indent=2)
            stream.write("\n")
    print("Prepared guarded patches only; no Kubernetes or kubeconfig operations were executed")


if __name__ == "__main__":
    main()
