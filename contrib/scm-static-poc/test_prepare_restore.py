#!/usr/bin/env python3
# Copyright Authors of Cilium
# SPDX-License-Identifier: Apache-2.0

import copy
import unittest

from prepare_restore import INPUT_BLOCK, NEWER_FLAGS, PARENT, guarded_patch, operator_args, prepare, producer_rules, static_config


class PrepareRestoreTests(unittest.TestCase):
    def test_operator_retains_unrelated_args_and_uses_only_historical_backend(self):
        baseline = ["--config-dir=/tmp/cilium/config-map", "--debug=$(CILIUM_DEBUG)",
                    "--enable-ztunnel-spiffeid-crd=true", "--mesh-auth-spiffe-trust-domain=spiffe.cilium"]
        current = baseline + [prefix + "old-value" for prefix in NEWER_FLAGS]
        result = operator_args(current)
        self.assertEqual(baseline, result[:len(baseline)])
        self.assertIn("--skip-crd-creation=true", result)
        self.assertIn("--mesh-auth-spire-crd-parent-id=" + PARENT, result)
        self.assertIn("--mesh-auth-spire-crd-class=cilium-spire", result)
        self.assertFalse(any(value.startswith(NEWER_FLAGS) for value in result))
        self.assertEqual(result, operator_args(result))
        with self.assertRaises(ValueError):
            operator_args(result + ["--mesh-auth-spire-crd-class=foreign"])
        with self.assertRaises(ValueError):
            operator_args(["--enable-ztunnel-spiffeid-crd=true"])

    def test_producer_grants_change_only_for_static_entry_mutations(self):
        before = [
            {"apiGroups": ["spire.spiffe.io"], "resources": ["clusterspiffeidsets", "clusterstaticentries"],
             "verbs": ["get", "list", "create", "update"]},
            {"apiGroups": [""], "resources": ["namespaces", "serviceaccounts"], "verbs": ["get", "list", "watch"]},
        ]
        baseline = copy.deepcopy(before)
        after = producer_rules(before)
        self.assertEqual(baseline, before)
        self.assertEqual(before[1:], after[1:])
        self.assertEqual(["clusterstaticentries"], after[0]["resources"])
        self.assertEqual(["get", "list", "create", "update", "delete"], after[0]["verbs"])
        self.assertEqual(after, producer_rules(after))
        with self.assertRaises(ValueError):
            producer_rules(before + [copy.deepcopy(before[0])])

    def test_config_preserves_scope_and_static_reconciliation(self):
        original = ('trustDomain: "spiffe.cilium"\nclassName: "cilium-spire"\nwatchClassless: false\n'
                    'entryIDPrefix: "cilium-scm-sa-poc-1108"\n' + INPUT_BLOCK +
                    'reconcile:\n  clusterSPIFFEIDs: false\n  clusterSPIFFEIDSets: true\n'
                    '  clusterStaticEntries: true\n  clusterFederatedTrustDomains: false\n')
        final = static_config(original)
        self.assertIn("  clusterStaticEntries: true\n", final)
        self.assertIn("  clusterSPIFFEIDSets: false\n", final)
        self.assertNotIn(INPUT_BLOCK, final)
        for invalid in (
            original.replace("clusterStaticEntries: true", "clusterStaticEntries: false"),
            original.replace("cilium-scm-sa-poc-1108", "different"),
            original + "entryIDPrefixCleanup: old\n",
            original.replace('kind: "ServiceAccount"', 'kind: "Pod"'),
        ):
            with self.subTest(config=invalid), self.assertRaises(ValueError):
                static_config(invalid)

    def test_patches_test_uid_version_and_each_original_value(self):
        obj = {"metadata": {"uid": "stable", "resourceVersion": "23"}, "data": {"a/b": "old"}, "rules": ["existing"]}
        baseline = copy.deepcopy(obj)
        patch = guarded_patch(obj, {"/data/a~1b": "new", "/rules": ["new"]})
        self.assertEqual(["test", "test", "test", "replace", "test", "replace"], [row["op"] for row in patch])
        self.assertEqual(["stable", "23", "old", "new", ["existing"], ["new"]], [row["value"] for row in patch])
        self.assertEqual(baseline, obj)

    def test_preparation_does_not_replace_unrelated_resource_fields(self):
        def obj(kind, name, namespace=None):
            metadata = {"name": name, "uid": name + "-uid", "resourceVersion": "23"}
            if namespace:
                metadata["namespace"] = namespace
            return {"kind": kind, "metadata": metadata}
        deployment = obj("Deployment", "cilium-operator", "kube-system")
        deployment["spec"] = {"replicas": 2, "template": {"spec": {"containers": [{
            "name": "cilium-operator", "image": "previous", "args": [
                "--enable-ztunnel-spiffeid-crd=true", "--mesh-auth-spiffe-trust-domain=spiffe.cilium"]}],
            "serviceAccountName": "cilium-operator", "hostNetwork": True}}}
        operator_role = obj("ClusterRole", "cilium-operator")
        operator_role["rules"] = [{"apiGroups": ["spire.spiffe.io"], "resources": ["clusterspiffeidsets", "clusterstaticentries"],
                                   "verbs": ["get", "list", "create", "update"]}]
        controller_role = obj("ClusterRole", "spire-server-controller-manager")
        controller_role["rules"] = [
            {"apiGroups": ["spire.spiffe.io"], "resources": ["clusterspiffeidsets", "clusterstaticentries"],
             "verbs": ["get", "list", "watch"]},
            {"apiGroups": ["spire.spiffe.io"], "resources": ["clusterspiffeidsets/status", "clusterstaticentries/status"],
             "verbs": ["get", "patch", "update"]},
            {"apiGroups": [""], "resources": ["namespaces"], "verbs": ["get", "list", "watch"]},
            {"apiGroups": [""], "resources": ["serviceaccounts"], "verbs": ["get", "list", "watch"]},
        ]
        config = obj("ConfigMap", "spire-server-crd-controller-manager", "cilium-spire")
        config["data"] = {"controller-manager.yaml": (
            'trustDomain: "spiffe.cilium"\nclassName: "cilium-spire"\nwatchClassless: false\n'
            'entryIDPrefix: "cilium-scm-sa-poc-1108"\n' + INPUT_BLOCK +
            'reconcile:\n  clusterSPIFFEIDs: false\n  clusterSPIFFEIDSets: true\n'
            '  clusterStaticEntries: true\n  clusterFederatedTrustDomains: false\n')}
        server = obj("StatefulSet", "spire-server-crd", "cilium-spire")
        server["spec"] = {"replicas": 2, "template": {"metadata": {"annotations": {
            "cilium.io/spire-controller-manager-configmap-checksum": "before", "other": "retained"}}}}
        saved = {"deployment": deployment, "operator_role": operator_role, "controller_config": config,
                 "controller_role": controller_role, "server": server}
        before = copy.deepcopy(saved)
        patches = prepare(saved, "localhost/scm-sa-poc/operator-generic:static-012345abcdef")
        self.assertEqual(before, saved)
        actual = {name: {row["path"] for row in patch if row["op"] == "replace"}
                  for name, patch in patches.items() if name.endswith(".patch.json")}
        self.assertEqual({
            "overlap-operator-deployment.patch.json": {"/spec/template/spec/containers/0/image", "/spec/template/spec/containers/0/args"},
            "overlap-operator-role.patch.json": {"/rules"},
            "static-only-config.patch.json": {"/data/controller-manager.yaml"},
            "static-only-role.patch.json": {"/rules"},
            "static-only-server.patch.json": {"/spec/template/metadata/annotations/cilium.io~1spire-controller-manager-configmap-checksum"},
        }, actual)
        self.assertFalse(patches["restore-summary.json"]["applies_to_cluster"])
        server["metadata"]["namespace"] = "wrong-target"
        with self.assertRaises(ValueError):
            prepare(saved, "localhost/scm-sa-poc/operator-generic:static-012345abcdef")


if __name__ == "__main__":
    unittest.main()
