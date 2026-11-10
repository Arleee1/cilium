#!/usr/bin/env python3
# Copyright Authors of Cilium
# SPDX-License-Identifier: Apache-2.0

import copy
import json
import pathlib
import tempfile
import unittest

import yaml

from prepare_minimal import (
    BOOTSTRAPS, CLASS, CLUSTER_UID, CONTEXT, ENDPOINT, NAMESPACE, OPTIONAL, PARENT, PREFIX,
    RELEASE, RELEASE_ANNOTATIONS, RETAINED, STATIC_RULES, check_target, expected_bootstrap_spec,
    normalized, prepare, write_results,
)


def fixture():
    def obj(kind, name, namespace="", **fields):
        api = "apps/v1" if kind == "Deployment" else "rbac.authorization.k8s.io/v1" if kind == "ClusterRole" else "v1"
        meta = {"name": name, "uid": name + "-uid", "resourceVersion": "23",
                "labels": {"app.kubernetes.io/managed-by": "Helm"}, "annotations": dict(RELEASE_ANNOTATIONS)}
        if namespace:
            meta["namespace"] = namespace
        return {"apiVersion": api, "kind": kind, "metadata": meta, **fields}

    snapshot = {
        "endpoint": ENDPOINT, "clusterUID": CLUSTER_UID, "observedAt": "2026-11-09T23:38:39-05:00",
        "bootstraps": {}, "staticEntries": {}, "enrolledPairs": [],
        "managerConfig": {
            "trustDomain": "spiffe.cilium", "className": CLASS, "watchClassless": False, "entryIDPrefix": PREFIX,
            "reconcile": {"clusterStaticEntries": True, "clusterSPIFFEIDSets": False,
                          "clusterSPIFFEIDs": False, "clusterFederatedTrustDomains": False},
        },
        "protected": {"workloadControllers": {}, "configMaps": {}, "secrets": {"cilium-spire/spire-upstream-ca": {"uid": "root"}}},
        "registrations": {},
    }
    for role, name in BOOTSTRAPS.items():
        saved = {
            "name": name, "uid": name + "-uid", "resourceVersion": "24", "spec": expected_bootstrap_spec(role),
            "labels": {"app.kubernetes.io/managed-by": "cilium-operator"}, "annotations": {},
        }
        snapshot["bootstraps"][role] = saved
        snapshot["staticEntries"][name] = copy.deepcopy(saved)
    for ns in ("sa-poc-a", "sa-poc-b"):
        for sa in ("app", "default", "unused"):
            pair = ns + "/" + sa
            snapshot["enrolledPairs"].append(pair)
            snapshot["staticEntries"]["ztunnel-" + ns + "." + sa] = {
                "uid": pair, "resourceVersion": "25",
                "spec": {"className": CLASS, "spiffeID": "spiffe://spiffe.cilium/ns/" + ns + "/sa/" + sa,
                         "parentID": PARENT, "selectors": ["k8s:ns:" + ns, "k8s:sa:" + sa]},
                "labels": {"app.kubernetes.io/managed-by": "cilium-operator-ztunnel", "spire.cilium.io/namespace": ns},
                "annotations": {"spire.cilium.io/service-account": pair},
            }
    for server in ("spire-server-crd-0", "spire-server-crd-1"):
        snapshot["registrations"][server] = [
            {"id": PREFIX + "." + server + "." + str(i), "revision_number": "0",
             "spiffe_id": {"trust_domain": "spiffe.cilium", "path": item["spec"]["spiffeID"].removeprefix("spiffe://spiffe.cilium")},
             "parent_id": {"trust_domain": "spiffe.cilium", "path": item["spec"]["parentID"].removeprefix("spiffe://spiffe.cilium")},
             "selectors": [{"type": s.split(":", 1)[0], "value": s.split(":", 1)[1]} for s in item["spec"]["selectors"]]}
            for i, item in enumerate(snapshot["staticEntries"].values())
        ]
    deployment = obj("Deployment", "cilium-operator", NAMESPACE, spec={"replicas": 2, "template": {"spec": {"containers": [{
        "name": "cilium-operator", "image": "localhost/scm-sa-poc/operator-generic:static-6ad602390271",
        "args": ["--config-dir=/tmp/cilium/config-map", "--debug=$(CILIUM_DEBUG)",
                 "--enable-ztunnel-spiffeid-crd=true", "--mesh-auth-spire-crd-parent-id=" + PARENT,
                 "--mesh-auth-spire-crd-class=" + CLASS, "--skip-crd-creation=true", "--log-level=debug"],
    }]}}})
    role = obj("ClusterRole", "spire-server-controller-manager", rules=copy.deepcopy(STATIC_RULES))
    config = obj("ConfigMap", "spire-server-crd-controller-manager", "cilium-spire",
                 data={"controller-manager.yaml": yaml.safe_dump(snapshot["managerConfig"])})
    secret = obj("Secret", "cilium-ca", NAMESPACE, data={"tls.crt": "fixture-only"}, type="Opaque")
    live = [deployment, role, config, secret]
    snapshot["protected"]["workloadControllers"]["deployment/kube-system/cilium-operator"] = {
        "uid": deployment["metadata"]["uid"], "spec": copy.deepcopy(deployment["spec"]),
    }
    snapshot["protected"]["clusterrole/spire-server-controller-manager"] = {
        "uid": role["metadata"]["uid"], "rules": copy.deepcopy(role["rules"]),
    }
    snapshot["protected"]["configMaps"]["cilium-spire/spire-server-crd-controller-manager"] = {
        "uid": config["metadata"]["uid"], "data": copy.deepcopy(config["data"]),
    }
    previous = copy.deepcopy(live)
    previous[0]["spec"]["template"]["spec"]["containers"][0]["image"] = "previous-set-producer"
    previous[0]["spec"]["template"]["spec"]["containers"][0]["args"].append("--enable-ztunnel-spiffeid-set-crd=true")
    values = {
        "encryption": {"ztunnel": {"spire": {"useCRD": True, "useSPIFFEIDSet": True, "spiffeIDSet": {"spec": "old"}},
                                  "extraEnv": [{"name": "SPIRE_ENABLED", "value": "true"}, {"name": "UNRELATED", "value": "keep"}]}},
        "operator": {"image": {"override": "previous-set-producer"}, "extraArgs": ["--stale"]},
        "authentication": {"mutual": {"spire": {"install": {"server": {"controllerManager": {"entryIDPrefix": PREFIX}}}}}},
    }
    capture = {
        "endpoint": ENDPOINT, "clusterUID": CLUSTER_UID, "context": CONTEXT,
        "release": RELEASE, "namespace": NAMESPACE, "revision": 6, "hooks": False, "read_only": True,
    }
    return snapshot, capture, values, previous, live


class PrepareMinimalTests(unittest.TestCase):
    def test_exact_minimal_and_recovery_profiles_preserve_all_existing_resources(self):
        args = fixture()
        before = copy.deepcopy(args)
        result = prepare(*args)
        self.assertEqual(before, args)
        for directory, roles in (("minimal-release", RETAINED), ("recovery-release", tuple(BOOTSTRAPS))):
            desired = list(yaml.safe_load_all(result[directory + "/resources.yaml"]))
            self.assertEqual([normalized(obj) for obj in args[-1]], desired[:4])
            self.assertEqual([BOOTSTRAPS[r] for r in roles], [obj["metadata"]["name"] for obj in desired[4:]])
            for role, obj in zip(roles, desired[4:]):
                self.assertEqual(expected_bootstrap_spec(role), obj["spec"])
                self.assertEqual("Helm", obj["metadata"]["labels"]["app.kubernetes.io/managed-by"])
                self.assertEqual(RELEASE_ANNOTATIONS, obj["metadata"]["annotations"])
            values = result[directory + "/values.yaml"]
            self.assertEqual("ztunnel" if directory == "minimal-release" else "legacy",
                             values["encryption"]["ztunnel"]["spire"]["bootstrapProfile"])
            self.assertEqual({"useCRD", "bootstrapProfile", "parentID", "className"},
                             values["encryption"]["ztunnel"]["spire"].keys())
            self.assertEqual(["--skip-crd-creation=true", "--log-level=debug"], values["operator"]["extraArgs"])
            self.assertEqual([{"name": "UNRELATED", "value": "keep"}], values["encryption"]["ztunnel"]["extraEnv"])
        self.assertFalse(result["migration-report.json"]["appliesToCluster"])
        self.assertFalse(result["migration-report.json"]["helmAligned"])
        self.assertFalse(result["migration-report.json"]["coldAttestationProven"])
        for server, entries in result["expected-registrations.json"].items():
            self.assertEqual(8, len(entries))
            self.assertEqual([e for e in args[0]["registrations"][server]
                              if e["spiffe_id"]["path"] not in ("/cilium-agent", "/cilium-operator")], entries)

    def test_ownership_and_deletion_are_exact_name_uid_version_and_spec_guarded(self):
        snapshot, *rest = fixture()
        result = prepare(snapshot, *rest)
        for role, name in BOOTSTRAPS.items():
            saved = snapshot["bootstraps"][role]
            folder = "adopt" if role in RETAINED else "adopt-if-present-for-recovery"
            patch = result[folder + "/" + name + ".patch.json"]
            self.assertEqual([
                {"op": "test", "path": "/metadata/uid", "value": saved["uid"]},
                {"op": "test", "path": "/metadata/resourceVersion", "value": saved["resourceVersion"]},
                {"op": "test", "path": "/spec", "value": saved["spec"]},
                {"op": "test", "path": "/metadata/labels", "value": saved["labels"]},
            ], patch[:4])
            if role in OPTIONAL:
                deletion = result["remove/" + name + ".request.json"]
                self.assertEqual("DELETE", deletion["method"])
                self.assertTrue(deletion["path"].endswith("/" + name))
                self.assertEqual(saved["spec"], deletion["expectedSpec"])
                self.assertEqual({"uid": saved["uid"], "resourceVersion": saved["resourceVersion"]},
                                 deletion["body"]["preconditions"])
        self.assertEqual(2, sum(name.startswith("remove/") for name in result))

    def test_rejects_protected_or_release_drift(self):
        for mutation in (
            lambda args: args[0].update(clusterUID="other"),
            lambda args: args[0]["bootstraps"]["operator"]["spec"].update(className="test-exclusion"),
            lambda args: args[0]["managerConfig"].update(entryIDPrefixCleanup="old"),
            lambda args: args[0]["registrations"]["spire-server-crd-1"].pop(),
            lambda args: args[0]["staticEntries"]["ztunnel-sa-poc-a.unused"]["spec"]["selectors"].append("k8s:pod-name:pod"),
            lambda args: args[1].update(revision=7),
            lambda args: args[1].update(hooks=True),
            lambda args: args[-1][0]["spec"].update(replicas=3),
            lambda args: args[-1][1]["rules"].append({"apiGroups": ["*"], "resources": ["*"], "verbs": ["*"]}),
            lambda args: args[-1][0]["metadata"]["annotations"].update({"meta.helm.sh/release-name": "other"}),
            lambda args: args[-1].pop(),
            lambda args: args[-2].append(copy.deepcopy(args[-2][0])),
        ):
            with self.subTest(mutation=mutation):
                args = fixture()
                mutation(args)
                with self.assertRaises((ValueError, KeyError)):
                    prepare(*args)

    def test_never_resurrects_set_or_schema_resources(self):
        for kind in ("ClusterSPIFFEIDSet", "ClusterServiceAccountSPIFFEID", "ClusterSPIFFEID",
                     "ClusterStaticEntry", "CustomResourceDefinition", "PersistentVolumeClaim"):
            args = fixture()
            obj = copy.deepcopy(args[-1][1])
            obj["kind"] = kind
            args[-2].append(obj)
            args[-1].append(copy.deepcopy(obj))
            with self.subTest(kind=kind), self.assertRaisesRegex(ValueError, "Unexpected release-managed"):
                prepare(*args)

    def test_full_target_comparison_rejects_unrelated_or_bootstrap_changes(self):
        args = fixture()
        result = prepare(*args)
        baseline = list(yaml.safe_load_all(result["minimal-release/resources.yaml"]))
        for mutation in (
            lambda objs: objs[0]["spec"]["template"]["spec"]["containers"][0].update(image="new-binary"),
            lambda objs: objs[1]["rules"].clear(),
            lambda objs: objs[3]["data"].update({"tls.crt": "different"}),
            lambda objs: objs[4]["spec"]["selectors"].append("different"),
            lambda objs: objs[4]["metadata"].update(name="renamed"),
            lambda objs: objs.pop(),
        ):
            desired = copy.deepcopy(baseline)
            mutation(desired)
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                check_target(args[0], args[-1], desired, RETAINED)

    def test_private_artifacts_cannot_overwrite_or_enter_checkout(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = pathlib.Path(temporary) / "private"
            write_results(output, {"chart/value.json": {"saved": True}})
            self.assertEqual({"saved": True}, json.loads((output / "chart/value.json").read_text()))
            with self.assertRaises(FileExistsError):
                write_results(output, {})
        with self.assertRaisesRegex(ValueError, "outside the source checkout"):
            write_results(pathlib.Path(__file__).resolve().parent / "do-not-create", {})


if __name__ == "__main__":
    unittest.main()
