#!/usr/bin/env python3
# Copyright Authors of Cilium
# SPDX-License-Identifier: Apache-2.0
"""Prepare private, offline POC bootstrap and Helm alignment artifacts. Never apply them."""

import argparse
import copy
import hashlib
import json
import os
import pathlib

import yaml

from prepare_restore import CLASS, NEWER_FLAGS, PARENT, PREFIX, STATIC_RULES, require


ENDPOINT = "https://aks-eermov-rg-eermovick-scm-9b8218-45zmt2zq.hcp.austriaeast.azmk8s.io:443"
CLUSTER_UID = "3d5ff43e-10de-4bfc-bb47-8c96859a42f0"
CONTEXT = "scm-sa-poc-1108"
RELEASE = "cilium"
NAMESPACE = "kube-system"
BOOTSTRAPS = {
    "agent-alias": "cilium-spire-kube-system-cilium-spire-agent",
    "ztunnel": "cilium-spire-kube-system-cilium-ztunnel",
    "cilium-agent": "cilium-spire-kube-system-cilium-cilium-agent",
    "operator": "cilium-spire-kube-system-cilium-cilium-operator",
}
RETAINED = ("agent-alias", "ztunnel")
OPTIONAL = ("cilium-agent", "operator")
OBSOLETE_VALUES = ("useSPIFFEIDSet", "spiffeIDSet", "useServiceAccountSPIFFEID", "workloadSelectorTemplates")
FORBIDDEN_KINDS = {
    "ClusterSPIFFEIDSet", "ClusterServiceAccountSPIFFEID", "ClusterSPIFFEID",
    "CustomResourceDefinition", "PersistentVolume", "PersistentVolumeClaim",
}
RELEASE_ANNOTATIONS = {
    "meta.helm.sh/release-name": RELEASE,
    "meta.helm.sh/release-namespace": NAMESPACE,
}
TEMPLATE = """{{- if or (not .Release.IsUpgrade) (ne .Release.Name "cilium") (ne .Release.Namespace "kube-system") -}}
{{- fail "This frozen POC chart is only for an explicitly guarded cilium upgrade in kube-system." -}}
{{- end -}}
{{- if ne (mustToJson .Values) (mustToJson (.Files.Get "profile-values.json" | mustFromJson)) -}}
{{- fail "Frozen POC values changed. Regenerate and review the complete migration; do not reuse Set-era values." -}}
{{- end -}}
{{- .Files.Get "resources.yaml" -}}
"""


def key(obj):
    meta = obj["metadata"]
    return (obj["apiVersion"], obj["kind"], meta.get("namespace", ""), meta["name"])


def normalized(obj):
    obj = copy.deepcopy(obj)
    require(not obj["metadata"].get("deletionTimestamp"), "Refuse a resource being deleted: " + str(key(obj)))
    obj.pop("status", None)
    for field in ("uid", "resourceVersion", "generation", "creationTimestamp", "managedFields", "selfLink"):
        obj["metadata"].pop(field, None)
    return obj


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def indexed(objects):
    result = {key(obj): obj for obj in objects}
    require(len(result) == len(objects), "Duplicate resource identity")
    return result


def expected_bootstrap_spec(role):
    if role == "agent-alias":
        return {
            "className": CLASS, "spiffeID": PARENT, "parentID": "spiffe://spiffe.cilium/spire/server",
            "selectors": ["k8s_psat:agent_ns:cilium-spire", "k8s_psat:agent_sa:spire-agent"],
        }
    identity, account = {
        "ztunnel": ("ztunnel", "ztunnel-cilium"),
        "cilium-agent": ("cilium-agent", "cilium"),
        "operator": ("cilium-operator", "cilium-operator"),
    }[role]
    return {
        "className": CLASS, "spiffeID": "spiffe://spiffe.cilium/" + identity, "parentID": PARENT,
        "selectors": ["k8s:ns:kube-system", "k8s:sa:" + account],
    }


def check_snapshot(snapshot):
    require(snapshot["endpoint"] == ENDPOINT and snapshot["clusterUID"] == CLUSTER_UID, "Wrong protected cluster")
    require(set(snapshot["bootstraps"]) == set(BOOTSTRAPS), "Restore the four-bootstrap baseline before preparing")
    for role, name in BOOTSTRAPS.items():
        obj = snapshot["bootstraps"][role]
        require(obj["name"] == name and obj["uid"] and obj["resourceVersion"], "Missing bootstrap identity: " + role)
        require(obj["spec"] == expected_bootstrap_spec(role), "Bootstrap spec/class changed: " + role)
        require(obj["labels"].get("app.kubernetes.io/managed-by") == "cilium-operator",
                "Unexpected bootstrap owner: " + role)
        require(not any(k.startswith("meta.helm.sh/") for k in obj["annotations"]), "Bootstrap already claimed by Helm")
    pairs = sorted(ns + "/" + sa for ns in ("sa-poc-a", "sa-poc-b") for sa in ("app", "default", "unused"))
    require(sorted(snapshot["enrolledPairs"]) == pairs, "Unexpected enrolled ServiceAccount inventory")
    workload_names = []
    for pair in pairs:
        ns, sa = pair.split("/")
        name = "ztunnel-" + ns + "." + sa
        workload_names.append(name)
        obj = snapshot["staticEntries"][name]
        require(obj["spec"] == {
            "className": CLASS, "spiffeID": "spiffe://spiffe.cilium/ns/" + ns + "/sa/" + sa,
            "parentID": PARENT, "selectors": ["k8s:ns:" + ns, "k8s:sa:" + sa],
        }, "Workload registration intent changed: " + name)
        require(obj["labels"].get("app.kubernetes.io/managed-by") == "cilium-operator-ztunnel"
                and obj["labels"].get("spire.cilium.io/namespace") == ns
                and obj["annotations"].get("spire.cilium.io/service-account") == pair,
                "Workload ownership changed: " + name)
    require(set(snapshot["staticEntries"]) == set(BOOTSTRAPS.values()) | set(workload_names),
            "Unexpected registration declarations; remove temporary proof resources first")
    for role, name in BOOTSTRAPS.items():
        obj = snapshot["bootstraps"][role]
        require(all(snapshot["staticEntries"][name][field] == obj[field]
                    for field in ("uid", "resourceVersion", "spec", "labels", "annotations")),
                "Inconsistent bootstrap snapshot: " + name)
    cfg = snapshot["managerConfig"]
    require(cfg["className"] == CLASS and cfg["watchClassless"] is False
            and cfg["entryIDPrefix"] == PREFIX and cfg["trustDomain"] == "spiffe.cilium"
            and "entryIDPrefixCleanup" not in cfg, "Preserve the deployed SCM scope")
    require(cfg["reconcile"] == {
        "clusterStaticEntries": True, "clusterSPIFFEIDSets": False,
        "clusterSPIFFEIDs": False, "clusterFederatedTrustDomains": False,
    }, "Static-only SCM is required")
    require(set(snapshot["registrations"]) == {"spire-server-crd-0", "spire-server-crd-1"}, "Check both independent stores")
    specs = {obj["spec"]["spiffeID"]: obj["spec"] for obj in snapshot["staticEntries"].values()}
    for server, entries in snapshot["registrations"].items():
        require(len(entries) == 10 and len({entry["id"] for entry in entries}) == 10, "Expected ten baseline registrations: " + server)
        seen = set()
        for entry in entries:
            sid = entry["spiffe_id"]
            identity = "spiffe://" + sid["trust_domain"] + sid["path"]
            parent = entry["parent_id"]
            spec = specs.get(identity)
            require(spec is not None and identity not in seen, "Unexpected or duplicate registration: " + server)
            seen.add(identity)
            require(entry["id"].startswith(PREFIX + ".") and ".identityset." not in entry["id"],
                    "Unadopted or stale Set registration: " + server)
            require("spiffe://" + parent["trust_domain"] + parent["path"] == spec["parentID"]
                    and sorted(s["type"] + ":" + s["value"] for s in entry["selectors"]) == sorted(spec["selectors"]),
                    "Registration differs from declaration: " + identity)


def check_live(snapshot, previous, live):
    old, current = indexed(previous), indexed(live)
    require(old.keys() == current.keys(), "Every recorded release resource must still exist; do not resurrect deleted objects")
    protected = snapshot["protected"]
    for identity, obj in current.items():
        api, kind, ns, name = identity
        require(kind not in FORBIDDEN_KINDS and kind != "ClusterStaticEntry", "Unexpected release-managed registration or protected resource: " + str(identity))
        require(not (kind == "Secret" and ns + "/" + name in protected["secrets"]), "Do not import externally owned root or ztunnel Secrets")
        require(obj["metadata"]["labels"].get("app.kubernetes.io/managed-by") == "Helm"
                and all(obj["metadata"]["annotations"].get(k) == v for k, v in RELEASE_ANNOTATIONS.items()),
                "Unexpected release ownership: " + str(identity))
        baseline = protected["workloadControllers"].get(kind.lower() + "/" + ns + "/" + name)
        fields = ("spec",)
        if kind == "ConfigMap":
            baseline = protected["configMaps"].get(ns + "/" + name)
            fields = ("data",)
        elif kind == "ClusterRole":
            baseline = protected.get("clusterrole/" + name)
            fields = ("rules",)
        if baseline is not None:
            require(obj["metadata"]["uid"] == baseline["uid"]
                    and all(obj.get(f) == baseline.get(f) for f in fields), "Protected baseline drift: " + str(identity))
        normalized(obj)
    operator = current[("apps/v1", "Deployment", "kube-system", "cilium-operator")]
    args = operator["spec"]["template"]["spec"]["containers"][0]["args"]
    require(not any(arg.startswith(NEWER_FLAGS) for arg in args), "Obsolete producer behavior must not be replayed")
    require(all(arg in args for arg in (
        "--enable-ztunnel-spiffeid-crd=true", "--mesh-auth-spire-crd-parent-id=" + PARENT,
        "--mesh-auth-spire-crd-class=" + CLASS, "--skip-crd-creation=true",
    )), "The restored CRD producer must already be running")
    role = current[("rbac.authorization.k8s.io/v1", "ClusterRole", "", "spire-server-controller-manager")]
    require(role["rules"] == STATIC_RULES, "SCM RBAC must remain Static-only")
    config = current[("v1", "ConfigMap", "cilium-spire", "spire-server-crd-controller-manager")]
    require(yaml.safe_load(config["data"]["controller-manager.yaml"]) == snapshot["managerConfig"], "SCM configuration disagreement")
    return current


def bootstrap_object(snapshot, role):
    saved = snapshot["bootstraps"][role]
    labels = dict(saved["labels"], **{"app.kubernetes.io/managed-by": "Helm"})
    annotations = dict(saved["annotations"], **RELEASE_ANNOTATIONS)
    return {
        "apiVersion": "spire.spiffe.io/v1alpha1", "kind": "ClusterStaticEntry",
        "metadata": {"name": saved["name"], "labels": labels, "annotations": annotations},
        "spec": copy.deepcopy(saved["spec"]),
    }


def adoption_patch(saved):
    return [
        {"op": "test", "path": "/metadata/uid", "value": saved["uid"]},
        {"op": "test", "path": "/metadata/resourceVersion", "value": saved["resourceVersion"]},
        {"op": "test", "path": "/spec", "value": saved["spec"]},
        {"op": "test", "path": "/metadata/labels", "value": saved["labels"]},
        {"op": "add", "path": "/metadata/labels/app.kubernetes.io~1managed-by", "value": "Helm"},
        {"op": "add", "path": "/metadata/annotations", "value": dict(saved["annotations"], **RELEASE_ANNOTATIONS)},
    ]


def profile_values(original, current, profile):
    values = copy.deepcopy(original)
    spire = values["encryption"]["ztunnel"]["spire"]
    for field in OBSOLETE_VALUES:
        spire.pop(field, None)
    spire.update(useCRD=True, bootstrapProfile=profile, parentID=PARENT, className=CLASS)
    values["authentication"]["mutual"]["spire"]["install"].setdefault("crds", {})["enabled"] = False
    values["authentication"]["mutual"]["spire"]["install"]["server"]["controllerManager"]["entryIDPrefix"] = PREFIX
    deployment = current[("apps/v1", "Deployment", NAMESPACE, "cilium-operator")]
    values["operator"]["image"]["override"] = deployment["spec"]["template"]["spec"]["containers"][0]["image"]
    values["operator"]["extraArgs"] = [
        arg for arg in deployment["spec"]["template"]["spec"]["containers"][0]["args"]
        if arg not in ("--config-dir=/tmp/cilium/config-map", "--debug=$(CILIUM_DEBUG)")
        and not arg.startswith(("--enable-ztunnel-spiffeid-crd=", "--mesh-auth-spire-crd-parent-id=", "--mesh-auth-spire-crd-class="))
    ]
    # Keep only unrelated external wiring in portable values. The frozen chart
    # preserves the complete deployed pod specs, including their original mounts.
    ztunnel = values["encryption"]["ztunnel"]
    env = {"SPIRE_ENABLED", "SPIRE_ADMIN_ENDPOINT_SOCKET", "CONTAINER_RUNTIME_SOCK_PATH", "TRUST_DOMAIN", "CLUSTER_DOMAIN"}
    for field, names in (
        ("extraEnv", env),
        ("extraVolumes", {"spire-admin", "containerd-socket"}),
        ("extraVolumeMounts", {"spire-admin", "containerd-socket"}),
    ):
        if field in ztunnel:
            ztunnel[field] = [item for item in ztunnel[field] if item["name"] not in names]
    return values


def check_target(snapshot, live, desired, roles):
    current, target = indexed(live), indexed(desired)
    bootstraps = [bootstrap_object(snapshot, role) for role in roles]
    expected = dict(current)
    expected.update(indexed(bootstraps))
    require(target.keys() == expected.keys(), "Target adds or removes an unintended resource")
    for identity, obj in target.items():
        require(normalized(obj) == normalized(expected[identity]), "Target spec/metadata drift: " + str(identity))


def prepare(snapshot, capture, original_values, previous, live):
    check_snapshot(snapshot)
    require(capture == {
        "endpoint": ENDPOINT, "clusterUID": CLUSTER_UID, "context": CONTEXT,
        "release": RELEASE, "namespace": NAMESPACE, "revision": 6, "hooks": False, "read_only": True,
    }, "Unexpected release capture; recapture and review the release transition")
    current = check_live(snapshot, previous, live)
    results = {}
    inventories = {}
    for directory, profile, roles in (
        ("minimal-release", "ztunnel", RETAINED),
        ("recovery-release", "legacy", tuple(BOOTSTRAPS)),
    ):
        desired = [normalized(obj) for obj in live] + [bootstrap_object(snapshot, role) for role in roles]
        check_target(snapshot, live, desired, roles)
        values = profile_values(original_values, current, profile)
        results[directory + "/Chart.yaml"] = {
            "apiVersion": "v2", "name": RELEASE, "version": "1.21.0-poc-static.1",
            "description": "Frozen, guarded POC release-state compatibility migration; not a general Cilium chart",
        }
        results[directory + "/values.yaml"] = values
        results[directory + "/profile-values.json"] = values
        results[directory + "/templates/resources.yaml"] = TEMPLATE
        results[directory + "/resources.yaml"] = "\n---\n".join(json.dumps(obj, indent=2) for obj in desired) + "\n"
        inventories[directory] = [
            {"identity": list(key(obj)), "sha256": digest(normalized(obj))} for obj in desired
        ]
    for role, name in BOOTSTRAPS.items():
        saved = snapshot["bootstraps"][role]
        directory = "adopt" if role in RETAINED else "adopt-if-present-for-recovery"
        results[directory + "/" + name + ".patch.json"] = adoption_patch(saved)
        if role in OPTIONAL:
            results["remove/" + name + ".request.json"] = {
                "method": "DELETE", "path": "/apis/spire.spiffe.io/v1alpha1/clusterstaticentries/" + name,
                "expectedSpec": saved["spec"],
                "body": {"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": {
                    "uid": saved["uid"], "resourceVersion": saved["resourceVersion"],
                }},
            }
    results["expected-registrations.json"] = {
        server: [entry for entry in entries if entry["spiffe_id"]["path"] not in ("/cilium-agent", "/cilium-operator")]
        for server, entries in snapshot["registrations"].items()
    }
    results["protected-baseline.json"] = snapshot
    results["preflight.json"] = {
        "cluster": capture,
        "releaseValuesSHA256": digest(original_values), "releaseManifestSHA256": digest(previous),
        "releaseResources": [{"identity": list(identity), "uid": obj["metadata"]["uid"],
                              "normalizedSHA256": digest(normalized(obj))} for identity, obj in current.items()],
        "bootstraps": snapshot["bootstraps"],
        "requireFreshResourceVersions": True,
    }
    results["migration-report.json"] = {
        "appliesToCluster": False, "helmAligned": False, "coldAttestationProven": False,
        "compatibilityMigrationOnly": True, "baselineObservedAt": snapshot["observedAt"],
        "existingReleaseResources": len(current), "inventories": inventories,
        "retainedBootstraps": [BOOTSTRAPS[role] for role in RETAINED],
        "optionalRemovals": [BOOTSTRAPS[role] for role in OPTIONAL],
        "expectedRegistrationsPerStore": {server: len(entries) for server, entries in results["expected-registrations.json"].items()},
        "existingResourceDrift": [],
        "helmUpdateStrategy": "--server-side=false",
        "forbiddenRecovery": ["helm --atomic", "helm --rollback-on-failure", "helm rollback to revision 6", "editing Helm release Secrets"],
        "requiredGates": [
            "Explicit coordinator authorization after fresh-node attestation and temporary-pool cleanup",
            "Fresh four-bootstrap snapshot with exact original UIDs/specs, roots/storage and protected workloads",
            "Dedicated kubeconfig, context, endpoint, cluster UID and unchanged revision 6 verified again",
            "Complete live normalized release inventory matches preflight; regenerate on any drift",
            "Apply only the two UID/resourceVersion-guarded ownership patches",
            "Forward Helm upgrade with minimal-release, reset values, server-side=false, no hooks, no CRDs and no automatic rollback",
            "Remove only the two optional CSEs using the exact DELETE bodies, with fresh spec and UID/version checks",
            "Both stores match all eight expected full registrations; workload CSE ownership/UID/specs unchanged",
            "Fresh issuance, cross-node authenticated traffic, renewal and unenrolled denial pass",
        ],
        "recovery": [
            "Use the frozen recovery-release as a compensating forward transaction, never the stale Set release",
            "If optional CSEs remain, regenerate their guarded Helm adoption patches from a fresh read",
            "If optional CSEs were deleted, recreate the exact saved names/specs through recovery-release; UIDs and entry IDs may change",
            "Keep retained CSE UIDs/specs, all workload CSEs, binaries, SCM prefix/configuration, roots and storage",
            "Verify four bootstraps and ten registrations on each store before declaring recovery",
        ],
    }
    return results


def write_results(output, results):
    require(not output.resolve().is_relative_to(pathlib.Path(__file__).resolve().parents[2]),
            "Private release manifests must be written outside the source checkout")
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    for name, value in results.items():
        path = output / name
        path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        descriptor = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        with os.fdopen(descriptor, "w") as stream:
            if isinstance(value, str):
                stream.write(value)
            else:
                json.dump(value, stream, indent=2)
                stream.write("\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", required=True, type=pathlib.Path)
    parser.add_argument("--release-capture", required=True, type=pathlib.Path)
    parser.add_argument("--output", required=True, type=pathlib.Path)
    args = parser.parse_args()
    def read(name):
        return json.loads((args.release_capture / name).read_text())

    previous = [obj for obj in yaml.safe_load_all((args.release_capture / "release-manifest.yaml").read_text()) if obj]
    history = read("release-history.json")
    require(history[-1]["revision"] == 6 and history[-1]["status"] == "deployed", "Expected deployed release revision 6")
    results = prepare(json.loads(args.baseline.read_text()), read("capture.json"),
                      read("release-values.json"), previous, read("release-live-objects.json"))
    write_results(args.output, results)
    print("Prepared private compatibility charts and guarded requests only; no live changes or Helm alignment")


if __name__ == "__main__":
    main()
