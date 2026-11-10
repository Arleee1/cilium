// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package spire

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cilium/hive/hivetest"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testBootstrapConfig(t *testing.T, domain, class string) ClientConfig {
	t.Helper()
	alias := "spiffe://" + domain + "/ns/cilium-spire/sa/spire-agent"
	entries := []bootstrapEntry{
		{Name: "cilium-spire-kube-system-cilium-spire-agent", Spec: bootstrapEntrySpec{
			SPIFFEID: alias, ParentID: "spiffe://" + domain + "/spire/server",
			Selectors: []string{"k8s_psat:agent_ns:cilium-spire", "k8s_psat:agent_sa:spire-agent"}, ClassName: class,
		}},
		{Name: "cilium-spire-kube-system-cilium-ztunnel", Spec: bootstrapEntrySpec{
			SPIFFEID: "spiffe://" + domain + "/ztunnel", ParentID: alias,
			Selectors: []string{"k8s:ns:kube-system", "k8s:sa:ztunnel-cilium"}, ClassName: class,
		}},
	}
	data, err := json.Marshal(entries)
	require.NoError(t, err)
	cfg := defaultClientConfig
	cfg.HAEnabled, cfg.SpiffeTrustDomain, cfg.CRDClassName = true, domain, class
	cfg.BootstrapEntries = string(data)
	return cfg
}

func establishedCRD(t *testing.T) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	crd, err := staticEntryCRD()
	require.NoError(t, err)
	crd.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{
		{Type: apiextensionsv1.NamesAccepted, Status: apiextensionsv1.ConditionTrue},
		{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue},
	}
	return crd
}

func haTestClient(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition) (*CRDClient, *apiextensionsfake.Clientset, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	entries, alias, err := parseBootstrapEntries(testBootstrapConfig(t, "spiffe.cilium", "cilium-spire"))
	require.NoError(t, err)
	var objects []runtime.Object
	if crd != nil {
		objects = append(objects, crd)
	}
	extensions := apiextensionsfake.NewSimpleClientset(objects...)
	dynamic := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	cfg := DefaultSpireEntryConfig()
	cfg.ParentID = alias.Path()
	return &CRDClient{
		client: dynamic.Resource(clusterStaticEntryGVR), crds: extensions.ApiextensionsV1().CustomResourceDefinitions(),
		logger: hivetest.Logger(t), bootstraps: entries, initialized: make(chan struct{}),
		trustDomain: "spiffe.cilium", className: "cilium-spire", entryCfg: cfg,
	}, extensions, dynamic
}

func TestHAStartupCreatesCRDBeforeBootstrapAndWorkloads(t *testing.T) {
	client, extensions, dynamic := haTestClient(t, nil)
	crdReady := false
	extensions.PrependReactor("get", "customresourcedefinitions", func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj, err := extensions.Tracker().Get(apiextensionsv1.SchemeGroupVersion.WithResource("customresourcedefinitions"), "", "clusterstaticentries.spire.spiffe.io")
		if apierrors.IsNotFound(err) {
			return false, nil, nil
		}
		require.NoError(t, err)
		crd := obj.(*apiextensionsv1.CustomResourceDefinition)
		crd.Status = establishedCRD(t).Status
		crdReady = true
		return true, crd, nil
	})
	dynamic.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		require.True(t, crdReady, "no dependent API calls before CRD establishment")
		require.Equal(t, "clusterstaticentries", action.GetResource().Resource, "startup must not enumerate Pods")
		return false, nil, nil
	})
	require.Empty(t, extensions.Actions())
	require.Empty(t, dynamic.Actions())
	select {
	case <-client.Initialized():
		t.Fatal("workload initialization must wait for leader startup")
	default:
	}
	require.NoError(t, client.onStart(t.Context()))
	<-client.Initialized()
	require.NoError(t, client.Upsert(t.Context(), "shop/unused"))
	var created []string
	for _, action := range dynamic.Actions() {
		if action.GetVerb() == "create" {
			created = append(created, action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured).GetName())
		}
	}
	require.Equal(t, []string{
		"cilium-spire-kube-system-cilium-spire-agent",
		"cilium-spire-kube-system-cilium-ztunnel",
		"ztunnel-shop.unused",
	}, created)
	var crdCreates int
	for _, action := range extensions.Actions() {
		if action.GetVerb() == "create" {
			crdCreates++
			require.Equal(t, "clusterstaticentries.spire.spiffe.io", action.(k8stesting.CreateAction).GetObject().(*apiextensionsv1.CustomResourceDefinition).Name)
		}
	}
	require.Equal(t, 1, crdCreates)
}

func TestHAStartupPreservesExistingObjectsAndRepeats(t *testing.T) {
	crd := establishedCRD(t)
	crd.UID, crd.ResourceVersion = "schema-uid", "42"
	crd.Labels = map[string]string{"app.kubernetes.io/managed-by": "external-schema-owner"}
	client, extensions, dynamic := haTestClient(t, crd)
	for _, desired := range client.bootstraps {
		obj := desired.DeepCopy()
		obj.SetUID(types.UID("retained-" + obj.GetName()))
		obj.SetResourceVersion("17")
		obj.SetAnnotations(map[string]string{"unrelated": "keep"})
		obj.SetFinalizers([]string{"example.test/retain"})
		obj.Object["status"] = map[string]any{"set": false, "rendered": true, "masked": false}
		require.NoError(t, dynamic.Tracker().Add(obj))
	}
	for range 2 {
		require.NoError(t, client.onStart(t.Context()))
	}
	for _, action := range append(extensions.Actions(), dynamic.Actions()...) {
		require.Equal(t, "get", action.GetVerb(), "repeat leader startup must not write matching objects")
	}
	// A new leader has a fresh readiness channel, but converges without writes.
	client.initialized = make(chan struct{})
	require.NoError(t, client.onStart(t.Context()))
	for _, action := range append(extensions.Actions(), dynamic.Actions()...) {
		require.Equal(t, "get", action.GetVerb())
	}
}

func TestHAStartupRejectsBootstrapConflicts(t *testing.T) {
	for _, tt := range []struct {
		name, want string
		change     func(*unstructured.Unstructured)
	}{
		{"foreign owner", "conflicting ownership", func(obj *unstructured.Unstructured) { obj.SetLabels(map[string]string{crdManagedByLabel: "Helm"}) }},
		{"Helm release", "conflicting ownership", func(obj *unstructured.Unstructured) {
			obj.SetAnnotations(map[string]string{"meta.helm.sh/release-name": "cilium"})
		}},
		{"Helm namespace", "conflicting ownership", func(obj *unstructured.Unstructured) {
			obj.SetAnnotations(map[string]string{"meta.helm.sh/release-namespace": "kube-system"})
		}},
		{"owner reference", "conflicting ownership", func(obj *unstructured.Unstructured) {
			obj.SetOwnerReferences([]metav1.OwnerReference{{Name: "other", UID: "other"}})
		}},
		{"deleting", "being deleted", func(obj *unstructured.Unstructured) { now := metav1.Now(); obj.SetDeletionTimestamp(&now) }},
		{"wrong selectors", "incompatible spec", func(obj *unstructured.Unstructured) {
			obj.Object["spec"].(map[string]any)["selectors"] = []any{"k8s:ns:other", "k8s:sa:ztunnel-cilium"}
		}},
		{"extra privilege", "incompatible spec", func(obj *unstructured.Unstructured) { obj.Object["spec"].(map[string]any)["admin"] = true }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, _, dynamic := haTestClient(t, establishedCRD(t))
			obj := client.bootstraps[1].DeepCopy()
			tt.change(obj)
			require.NoError(t, dynamic.Tracker().Add(obj))
			require.ErrorContains(t, client.onStart(t.Context()), tt.want)
			for _, action := range dynamic.Actions() {
				require.Equal(t, "get", action.GetVerb(), "validate both existing entries before any creation")
			}
			select {
			case <-client.Initialized():
				t.Fatal("failed bootstrap must not release workload reconciliation")
			default:
			}
		})
	}
}

func TestHAStartupCRDErrors(t *testing.T) {
	for _, tt := range []struct {
		name, want string
		change     func(*apiextensionsv1.CustomResourceDefinition)
	}{
		{"schema conflict", "differs from the embedded schema", func(crd *apiextensionsv1.CustomResourceDefinition) { crd.Spec.Versions[0].Schema = nil }},
		{"unserved version", "differs from the embedded schema", func(crd *apiextensionsv1.CustomResourceDefinition) { crd.Spec.Versions[0].Served = false }},
		{"wrong scope", "differs from the embedded schema", func(crd *apiextensionsv1.CustomResourceDefinition) { crd.Spec.Scope = apiextensionsv1.NamespaceScoped }},
		{"deleting", "being deleted", func(crd *apiextensionsv1.CustomResourceDefinition) { now := metav1.Now(); crd.DeletionTimestamp = &now }},
		{"name conflict", "names not accepted", func(crd *apiextensionsv1.CustomResourceDefinition) {
			crd.Status.Conditions[0].Status = apiextensionsv1.ConditionFalse
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			crd := establishedCRD(t)
			tt.change(crd)
			client, extensions, dynamic := haTestClient(t, crd)
			require.ErrorContains(t, client.onStart(t.Context()), tt.want)
			require.Empty(t, dynamic.Actions())
			require.Len(t, extensions.Actions(), 1)
			require.Equal(t, "get", extensions.Actions()[0].GetVerb())
		})
	}
	for _, operation := range []string{"get", "create"} {
		t.Run("RBAC "+operation, func(t *testing.T) {
			client, extensions, dynamic := haTestClient(t, nil)
			extensions.PrependReactor(operation, "customresourcedefinitions", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, "clusterstaticentries.spire.spiffe.io", errors.New("RBAC denied"))
			})
			require.ErrorContains(t, client.onStart(t.Context()), "RBAC denied")
			require.Empty(t, dynamic.Actions())
		})
	}
}

func TestHAStartupRetriesRacesAndTransientErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"create race", apierrors.NewAlreadyExists(clusterStaticEntryGVR.GroupResource(), "bootstrap")},
		{"throttled", apierrors.NewTooManyRequests("retry startup", 0)},
		{"unavailable", apierrors.NewServiceUnavailable("retry startup")},
		{"timeout", apierrors.NewTimeoutError("retry startup", 0)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, _, dynamic := haTestClient(t, establishedCRD(t))
			calls := 0
			dynamic.PrependReactor("create", "clusterstaticentries", func(action k8stesting.Action) (bool, runtime.Object, error) {
				calls++
				if calls == 1 {
					return true, nil, tt.err
				}
				return false, nil, nil
			})
			require.NoError(t, client.onStart(t.Context()))
			require.Equal(t, 3, calls)
		})
	}
	t.Run("CRD creation race", func(t *testing.T) {
		client, extensions, _ := haTestClient(t, nil)
		extensions.PrependReactor("create", "customresourcedefinitions", func(k8stesting.Action) (bool, runtime.Object, error) {
			require.NoError(t, extensions.Tracker().Add(establishedCRD(t)))
			return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "customresourcedefinitions"}, "clusterstaticentries.spire.spiffe.io")
		})
		require.NoError(t, client.onStart(t.Context()))
	})
	t.Run("established but not served yet", func(t *testing.T) {
		client, _, dynamic := haTestClient(t, establishedCRD(t))
		first := true
		dynamic.PrependReactor("create", "clusterstaticentries", func(k8stesting.Action) (bool, runtime.Object, error) {
			if first {
				first = false
				return true, nil, apierrors.NewNotFound(clusterStaticEntryGVR.GroupResource(), "")
			}
			return false, nil, nil
		})
		require.NoError(t, client.onStart(t.Context()))
	})
}

func TestHAStartupCancellationAndBootstrapRBAC(t *testing.T) {
	crd := establishedCRD(t)
	crd.Status.Conditions = nil
	client, _, dynamic := haTestClient(t, crd)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, client.onStart(ctx), context.DeadlineExceeded)
	require.Empty(t, dynamic.Actions())
	for _, verb := range []string{"get", "create"} {
		t.Run(verb, func(t *testing.T) {
			client, _, dynamic := haTestClient(t, establishedCRD(t))
			dynamic.PrependReactor(verb, "clusterstaticentries", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(clusterStaticEntryGVR.GroupResource(), "bootstrap", errors.New("bootstrap RBAC denied"))
			})
			require.ErrorContains(t, client.onStart(t.Context()), "bootstrap RBAC denied")
			select {
			case <-client.Initialized():
				t.Fatal("bootstrap RBAC failure must block initialization")
			default:
			}
		})
	}
}

func TestBootstrapConfigValidation(t *testing.T) {
	cfg := testBootstrapConfig(t, "spiffe.cilium", "cilium-spire")
	for _, bad := range []string{
		"", "null", "[]", "{}", cfg.BootstrapEntries + " []",
		strings.Replace(cfg.BootstrapEntries, `"name":`, `"unknown":true,"name":`, 1),
		strings.Replace(cfg.BootstrapEntries, `"spiffeID":`, `"admin":true,"spiffeID":`, 1),
		strings.Replace(cfg.BootstrapEntries, "cilium-spire-kube-system-cilium-spire-agent", "Bad_Name", 1),
		strings.Replace(cfg.BootstrapEntries, "spiffe://spiffe.cilium/spire/server", "spiffe://other.test/spire/server", 1),
		strings.ReplaceAll(cfg.BootstrapEntries, "spiffe://spiffe.cilium", "spiffe://other.test"),
		strings.Replace(cfg.BootstrapEntries, "k8s_psat:agent_ns:cilium-spire", "k8s_psat:cluster:other", 1),
		strings.Replace(cfg.BootstrapEntries, "k8s:sa:ztunnel-cilium", "k8s:pod-name:ztunnel", 1),
		strings.Replace(cfg.BootstrapEntries, "cilium-spire-kube-system-cilium-ztunnel", "cilium-spire-kube-system-cilium-spire-agent", 1),
		strings.Replace(cfg.BootstrapEntries, `"className":"cilium-spire"`, `"className":"other"`, 1),
	} {
		t.Run(bad, func(t *testing.T) {
			invalid := cfg
			invalid.BootstrapEntries = bad
			_, _, err := parseBootstrapEntries(invalid)
			require.Error(t, err)
		})
	}
	var entries []bootstrapEntry
	require.NoError(t, json.Unmarshal([]byte(cfg.BootstrapEntries), &entries))
	entries[0].Spec.Selectors = append(entries[0].Spec.Selectors, "k8s_psat:cluster:lab-test")
	data, err := json.Marshal(entries)
	require.NoError(t, err)
	cfg.BootstrapEntries = string(data)
	objects, _, err := parseBootstrapEntries(cfg)
	require.NoError(t, err)
	require.Len(t, objects, 2)
	require.Len(t, objects[0].Object["spec"].(map[string]any)["selectors"], 3)
}
