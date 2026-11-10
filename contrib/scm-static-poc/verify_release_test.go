// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestVerifyExistingThreeWayMerge(t *testing.T) {
	for _, tt := range []struct {
		api, kind, field         string
		before, restored, unsafe any
	}{
		{"v1", "ConfigMap", "data", map[string]any{"mode": "set"}, map[string]any{"mode": "static"}, map[string]any{"mode": "set"}},
		{"v1", "Secret", "data", map[string]any{"tls.crt": "c2FtZQ=="}, map[string]any{"tls.crt": "c2FtZQ=="}, map[string]any{"tls.crt": "bmV3"}},
		{"spire.spiffe.io/v1alpha1", "ClusterStaticEntry", "spec",
			map[string]any{"className": "cilium-spire"}, map[string]any{"className": "cilium-spire"}, map[string]any{"className": "test-exclusion"}},
		{"apps/v1", "Deployment", "spec",
			map[string]any{"replicas": int64(2), "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "operator", "image": "set-producer"}}}}},
			map[string]any{"replicas": int64(2), "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "operator", "image": "static-producer"}}}}},
			map[string]any{"replicas": int64(2), "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "operator", "image": "unexpected"}}}}}},
		{"rbac.authorization.k8s.io/v1", "ClusterRole", "rules",
			[]any{map[string]any{"apiGroups": []any{"spire.spiffe.io"}, "resources": []any{"clusterspiffeidsets"}, "verbs": []any{"get"}}},
			[]any{map[string]any{"apiGroups": []any{"spire.spiffe.io"}, "resources": []any{"clusterstaticentries"}, "verbs": []any{"get"}}},
			[]any{map[string]any{"apiGroups": []any{"*"}, "resources": []any{"*"}, "verbs": []any{"*"}}}},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			old := unstructured.Unstructured{Object: map[string]any{
				"apiVersion": tt.api, "kind": tt.kind, "metadata": map[string]any{"name": "retained"}, tt.field: tt.before,
			}}
			current := old.DeepCopy()
			current.Object[tt.field] = tt.restored
			current.SetResourceVersion("42")
			current.SetUID("retained-uid")
			desired := current.DeepCopy()
			desired.SetResourceVersion("")
			desired.SetUID("")
			require.NoError(t, verifyExisting(old, *current, *desired))
			desired.Object[tt.field] = tt.unsafe
			require.ErrorContains(t, verifyExisting(old, *current, *desired), "would change an existing resource")
		})
	}
}
