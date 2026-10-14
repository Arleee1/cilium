// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package spire

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/cilium/hive/hivetest"
	entryv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/server/entry/v1"
	"github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

// mockCRDResource models optimistic concurrency as well as the CRUD operations.
// Embedding ResourceInterface makes unexpected operations fail in tests.
type mockCRDResource struct {
	dynamic.ResourceInterface
	objects       map[string]*unstructured.Unstructured
	calls         []string
	before        func(verb, name string) error
	list          func(metav1.ListOptions) (*unstructured.UnstructuredList, error)
	deleteOptions []metav1.DeleteOptions
	version       int
}

func newTestCRDClient() (*CRDClient, *mockCRDResource) {
	resource := &mockCRDResource{objects: map[string]*unstructured.Unstructured{}}
	return &CRDClient{
		client:      resource,
		trustDomain: "test.cilium",
		entryCfg:    ZtunnelSpireEntryConfig(),
		initialized: make(chan struct{}),
	}, resource
}

func (m *mockCRDResource) call(verb, name string) error {
	m.calls = append(m.calls, verb+":"+name)
	if m.before != nil {
		return m.before(verb, name)
	}
	return nil
}

func (m *mockCRDResource) Get(_ context.Context, name string, _ metav1.GetOptions, _ ...string) (*unstructured.Unstructured, error) {
	if err := m.call("get", name); err != nil {
		return nil, err
	}
	obj, ok := m.objects[name]
	if !ok {
		return nil, apierrors.NewNotFound(clusterStaticEntryGVR.GroupResource(), name)
	}
	return obj.DeepCopy(), nil
}

func (m *mockCRDResource) Create(_ context.Context, obj *unstructured.Unstructured, _ metav1.CreateOptions, _ ...string) (*unstructured.Unstructured, error) {
	name := obj.GetName()
	if err := m.call("create", name); err != nil {
		return nil, err
	}
	if _, exists := m.objects[name]; exists {
		return nil, apierrors.NewAlreadyExists(clusterStaticEntryGVR.GroupResource(), name)
	}
	m.version++
	obj = obj.DeepCopy()
	obj.SetUID(k8stypes.UID(fmt.Sprintf("uid-%d", m.version)))
	obj.SetResourceVersion(fmt.Sprint(m.version))
	m.objects[name] = obj
	return obj.DeepCopy(), nil
}

func (m *mockCRDResource) Update(_ context.Context, obj *unstructured.Unstructured, _ metav1.UpdateOptions, _ ...string) (*unstructured.Unstructured, error) {
	name := obj.GetName()
	if err := m.call("update", name); err != nil {
		return nil, err
	}
	current, ok := m.objects[name]
	if !ok || obj.GetResourceVersion() != current.GetResourceVersion() || obj.GetUID() != current.GetUID() {
		return nil, apierrors.NewConflict(clusterStaticEntryGVR.GroupResource(), name, errors.New("object changed"))
	}
	m.version++
	obj = obj.DeepCopy()
	obj.SetResourceVersion(fmt.Sprint(m.version))
	m.objects[name] = obj
	return obj.DeepCopy(), nil
}

func (m *mockCRDResource) Delete(_ context.Context, name string, opts metav1.DeleteOptions, _ ...string) error {
	m.deleteOptions = append(m.deleteOptions, opts)
	if err := m.call("delete", name); err != nil {
		return err
	}
	obj, ok := m.objects[name]
	if !ok {
		return apierrors.NewNotFound(clusterStaticEntryGVR.GroupResource(), name)
	}
	if pre := opts.Preconditions; pre != nil {
		if pre.UID != nil && *pre.UID != obj.GetUID() ||
			pre.ResourceVersion != nil && *pre.ResourceVersion != obj.GetResourceVersion() {
			return apierrors.NewConflict(clusterStaticEntryGVR.GroupResource(), name, errors.New("object changed"))
		}
	}
	delete(m.objects, name)
	return nil
}

func (m *mockCRDResource) List(_ context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	if err := m.call("list", ""); err != nil {
		return nil, err
	}
	if m.list != nil {
		return m.list(opts)
	}
	selector, err := labels.Parse(opts.LabelSelector)
	if err != nil {
		return nil, err
	}
	list := &unstructured.UnstructuredList{}
	for _, name := range slices.Sorted(maps.Keys(m.objects)) {
		obj := m.objects[name]
		if selector.Matches(labels.Set(obj.GetLabels())) {
			list.Items = append(list.Items, *obj.DeepCopy())
		}
	}
	return list, nil
}

func TestCRDClient_ServiceAccountParity(t *testing.T) {
	c, resource := newTestCRDClient()
	ids := []string{"shop/checkout", "shop/catalog", "other/checkout"}
	var entries []*types.Entry
	direct := &Client{
		cfg:      ClientConfig{SpiffeTrustDomain: c.trustDomain},
		entryCfg: ZtunnelSpireEntryConfig(),
		log:      hivetest.Logger(t),
		entry: mockEntryClient{
			BatchCreateEntryFunc: func(_ context.Context, req *entryv1.BatchCreateEntryRequest, _ ...grpc.CallOption) (*entryv1.BatchCreateEntryResponse, error) {
				entries = req.Entries
				return &entryv1.BatchCreateEntryResponse{}, nil
			},
		},
	}
	require.NoError(t, direct.UpsertBatch(t.Context(), ids))
	require.NoError(t, c.UpsertBatch(t.Context(), ids))
	require.Len(t, resource.objects, len(ids))
	for i, id := range ids {
		obj := resource.objects[crdEntryName(id)]
		require.Equal(t, "ClusterStaticEntry", obj.GetKind())
		require.Equal(t, "spire.spiffe.io/v1alpha1", obj.GetAPIVersion())
		require.Empty(t, obj.GetOwnerReferences())
		require.Equal(t, id, obj.GetAnnotations()[crdIDAnnotation])
		spec, found, err := unstructured.NestedMap(obj.Object, "spec")
		require.NoError(t, err)
		require.True(t, found)
		entry := entries[i]
		require.Equal(t, "spiffe://"+entry.SpiffeId.TrustDomain+entry.SpiffeId.Path, spec["spiffeID"])
		require.Equal(t, "spiffe://"+entry.ParentId.TrustDomain+entry.ParentId.Path, spec["parentID"])
		require.Equal(t, "/ztunnel", entry.ParentId.Path)
		selectors := []any{}
		for _, selector := range entry.Selectors {
			selectors = append(selectors, selector.Type+":"+selector.Value)
		}
		require.Equal(t, selectors, spec["selectors"])
	}
	resource.calls = nil
	require.NoError(t, c.UpsertBatch(t.Context(), ids))
	require.NoError(t, c.Upsert(t.Context(), ids[0]))
	require.Len(t, resource.calls, 4)
	for _, call := range resource.calls {
		require.True(t, strings.HasPrefix(call, "get:"), call)
	}
}

func TestCRDEntryNamesAndInvalidIDs(t *testing.T) {
	ids := []string{
		"a-b/c", "a/b-c", "a/b.c",
		strings.Repeat("n", 63) + "/" + strings.Repeat("s", 253),
		strings.Repeat("n", 63) + "/" + strings.Repeat("s", 252) + "t",
	}
	names := map[string]bool{}
	for _, id := range ids {
		name := crdEntryName(id)
		require.Equal(t, name, crdEntryName(id))
		require.LessOrEqual(t, len(name), maxCRDNameLen)
		require.Empty(t, validation.IsDNS1123Label(name))
		require.False(t, names[name], id)
		names[name] = true
	}
	c, resource := newTestCRDClient()
	require.NoError(t, c.Upsert(t.Context(), ids[3]))
	require.Equal(t, ids[3], resource.objects[crdEntryName(ids[3])].GetAnnotations()[crdIDAnnotation])
	resource.calls = nil
	for _, id := range []string{"", "shop", "shop/", "/sa", "a/b/c", "Upper/sa", "ns/Upper", strings.Repeat("n", 64) + "/sa", "ns/" + strings.Repeat("s", 254)} {
		require.Error(t, c.Upsert(t.Context(), id), id)
		require.Error(t, c.Delete(t.Context(), id), id)
	}
	require.NoError(t, c.UpsertBatch(t.Context(), nil))
	require.NoError(t, c.DeleteBatch(t.Context(), nil))
	require.Error(t, c.Prune(t.Context(), "bad/namespace", func(string) bool { return false }))
	require.Error(t, c.Prune(t.Context(), "", nil))
	require.Empty(t, resource.calls)
}

func TestCRDClient_IsolatedDeletion(t *testing.T) {
	c, resource := newTestCRDClient()
	require.NoError(t, c.UpsertBatch(t.Context(), []string{"shop/checkout", "shop/catalog", "other/checkout"}))
	deleted := resource.objects[crdEntryName("shop/checkout")].DeepCopy()
	require.NoError(t, c.Delete(t.Context(), "shop/checkout"))
	require.NoError(t, c.Delete(t.Context(), "shop/checkout"))
	require.Len(t, resource.objects, 2)
	require.Contains(t, resource.objects, crdEntryName("shop/catalog"))
	require.Equal(t, deleted.GetUID(), *resource.deleteOptions[0].Preconditions.UID)
	require.Equal(t, deleted.GetResourceVersion(), *resource.deleteOptions[0].Preconditions.ResourceVersion)
	require.NoError(t, c.DeleteBatch(t.Context(), []string{"shop/catalog"}))
	require.Len(t, resource.objects, 1)
	require.Contains(t, resource.objects, crdEntryName("other/checkout"))
}

func TestCRDClient_PreservesControllerFields(t *testing.T) {
	c, resource := newTestCRDClient()
	id := "shop/checkout"
	require.NoError(t, c.Upsert(t.Context(), id))
	obj := resource.objects[crdEntryName(id)]
	uid := obj.GetUID()
	obj.SetFinalizers([]string{"spire.spiffe.io/entry"})
	obj.SetLabels(maps.Clone(obj.GetLabels()))
	labels := obj.GetLabels()
	labels["other.example/label"] = "keep"
	obj.SetLabels(labels)
	annotations := obj.GetAnnotations()
	annotations["other.example/annotation"] = "keep"
	obj.SetAnnotations(annotations)
	require.NoError(t, unstructured.SetNestedField(obj.Object, "spire", "spec", "className"))
	require.NoError(t, unstructured.SetNestedField(obj.Object, "old", "spec", "parentID"))
	obj.Object["status"] = map[string]any{"entryID": "opaque-id"}
	require.NoError(t, c.Upsert(t.Context(), id))
	updated := resource.objects[crdEntryName(id)]
	require.Equal(t, uid, updated.GetUID())
	require.Equal(t, obj.GetFinalizers(), updated.GetFinalizers())
	require.Equal(t, obj.GetLabels(), updated.GetLabels())
	require.Equal(t, obj.GetAnnotations(), updated.GetAnnotations())
	require.Equal(t, obj.Object["status"], updated.Object["status"])
	spec := updated.Object["spec"].(map[string]any)
	require.Equal(t, "spire", spec["className"])
	require.Equal(t, "spiffe://test.cilium/ztunnel", spec["parentID"])
}

func TestCRDClient_RefusesForeignOwnership(t *testing.T) {
	for _, mutation := range []string{"managed-by", "purpose", "namespace", "account"} {
		t.Run(mutation, func(t *testing.T) {
			c, resource := newTestCRDClient()
			id := "shop/checkout"
			require.NoError(t, c.Upsert(t.Context(), id))
			obj := resource.objects[crdEntryName(id)]
			labels := obj.GetLabels()
			switch mutation {
			case "managed-by":
				labels[crdManagedByLabel] = "another-operator"
			case "purpose":
				delete(labels, crdPurposeLabel)
			case "namespace":
				labels[crdNamespaceLabel] = "other"
			case "account":
				obj.SetAnnotations(map[string]string{crdIDAnnotation: "shop/catalog"})
			}
			obj.SetLabels(labels)
			before := obj.DeepCopy()
			require.ErrorContains(t, c.Upsert(t.Context(), id), "not an owned")
			require.ErrorContains(t, c.Delete(t.Context(), id), "not an owned")
			require.Equal(t, before, resource.objects[obj.GetName()])
		})
	}
}

func TestCRDClient_BatchPartialFailures(t *testing.T) {
	c, resource := newTestCRDClient()
	ids := []string{"shop/one", "shop/two", "shop/three"}
	first, second := errors.New("first failure"), errors.New("second failure")
	failures := map[string]error{crdEntryName(ids[0]): first, crdEntryName(ids[2]): second}
	for _, verb := range []string{"create", "delete"} {
		t.Run(verb, func(t *testing.T) {
			resource.before = func(operation, name string) error {
				if operation == verb {
					return failures[name]
				}
				return nil
			}
			var err error
			if verb == "create" {
				err = c.UpsertBatch(t.Context(), ids)
			} else {
				err = c.DeleteBatch(t.Context(), ids)
			}
			require.ErrorIs(t, err, first)
			require.ErrorIs(t, err, second)
			require.ErrorContains(t, err, ids[0])
			require.ErrorContains(t, err, ids[2])
			resource.before = nil
			if verb == "create" {
				require.Len(t, resource.objects, 1)
				require.Contains(t, resource.objects, crdEntryName(ids[1]))
				require.NoError(t, c.UpsertBatch(t.Context(), ids))
			} else {
				require.Len(t, resource.objects, 2)
				require.NotContains(t, resource.objects, crdEntryName(ids[1]))
				require.NoError(t, c.DeleteBatch(t.Context(), ids))
				require.Empty(t, resource.objects)
			}
		})
	}
}

func TestCRDClient_ConcurrentChanges(t *testing.T) {
	for _, verb := range []string{"create", "update", "delete", "prune"} {
		t.Run(verb, func(t *testing.T) {
			c, resource := newTestCRDClient()
			id, name := "shop/checkout", crdEntryName("shop/checkout")
			if verb != "create" {
				require.NoError(t, c.Upsert(t.Context(), id))
				require.NoError(t, unstructured.SetNestedField(resource.objects[name].Object, "old", "spec", "parentID"))
			}
			resource.before = func(operation, _ string) error {
				if operation == verb || verb == "prune" && operation == "delete" {
					replacement, err := c.buildClusterStaticEntry(id)
					require.NoError(t, err)
					replacement.SetLabels(map[string]string{crdManagedByLabel: "someone-else"})
					replacement.SetUID("new-uid")
					replacement.SetResourceVersion("999")
					resource.objects[name] = replacement
				}
				return nil
			}
			var err error
			switch verb {
			case "create", "update":
				err = c.Upsert(t.Context(), id)
			case "delete":
				err = c.Delete(t.Context(), id)
			case "prune":
				err = c.Prune(t.Context(), "", func(string) bool { return false })
			}
			require.Error(t, err)
			require.Equal(t, "someone-else", resource.objects[name].GetLabels()[crdManagedByLabel])
			resource.before = nil
			require.ErrorContains(t, c.Upsert(t.Context(), id), "not an owned")
			require.ErrorContains(t, c.Delete(t.Context(), id), "not an owned")
		})
	}
}

func TestCRDClient_TerminatingEntry(t *testing.T) {
	c, resource := newTestCRDClient()
	id := "shop/checkout"
	require.NoError(t, c.Upsert(t.Context(), id))
	now := metav1.Now()
	resource.objects[crdEntryName(id)].SetDeletionTimestamp(&now)
	require.ErrorContains(t, c.Upsert(t.Context(), id), "still being deleted")
	require.NoError(t, c.Delete(t.Context(), id))
	require.NoError(t, c.Upsert(t.Context(), id))
}

func TestCRDClient_PruneOwnershipAndScope(t *testing.T) {
	c, resource := newTestCRDClient()
	require.NoError(t, c.UpsertBatch(t.Context(), []string{"shop/keep", "shop/stale", "other/stale"}))
	for _, name := range []string{"ztunnel", "spire-agent-node-alias", "old-poc-entry"} {
		obj := &unstructured.Unstructured{}
		obj.SetName(name)
		obj.SetLabels(map[string]string{crdManagedByLabel: crdManagedByValue})
		resource.objects[name] = obj
	}
	var checked []string
	require.NoError(t, c.Prune(t.Context(), "shop", func(id string) bool {
		checked = append(checked, id)
		return id == "shop/keep"
	}))
	require.ElementsMatch(t, []string{"shop/keep", "shop/stale"}, checked)
	require.NotContains(t, resource.objects, crdEntryName("shop/stale"))
	require.Contains(t, resource.objects, crdEntryName("other/stale"))
	require.NoError(t, c.Prune(t.Context(), "", func(id string) bool { return id == "shop/keep" }))
	require.Len(t, resource.objects, 4)
	require.Contains(t, resource.objects, crdEntryName("shop/keep"))
}

func TestCRDClient_PrunePaginationAndFailures(t *testing.T) {
	c, resource := newTestCRDClient()
	require.NoError(t, c.UpsertBatch(t.Context(), []string{"shop/one", "shop/two"}))
	pages := 0
	resource.list = func(opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
		require.EqualValues(t, 500, opts.Limit)
		require.Contains(t, opts.LabelSelector, crdPurposeLabel+"="+crdPurposeValue)
		list := &unstructured.UnstructuredList{}
		if pages == 0 {
			require.Empty(t, opts.Continue)
			list.Items = []unstructured.Unstructured{*resource.objects[crdEntryName("shop/one")].DeepCopy()}
			list.SetContinue("second-page")
		} else {
			require.Equal(t, "second-page", opts.Continue)
			list.Items = []unstructured.Unstructured{*resource.objects[crdEntryName("shop/two")].DeepCopy()}
		}
		pages++
		return list, nil
	}
	failure := errors.New("deletion failed")
	resource.before = func(verb, name string) error {
		if verb == "delete" && name == crdEntryName("shop/one") {
			return failure
		}
		return nil
	}
	require.ErrorIs(t, c.Prune(t.Context(), "", func(string) bool { return false }), failure)
	require.Equal(t, 2, pages)
	require.Len(t, resource.objects, 1)
	resource.list = nil
	resource.before = func(verb, _ string) error {
		if verb == "list" || verb == "get" {
			return failure
		}
		return nil
	}
	require.ErrorIs(t, c.Prune(t.Context(), "", func(string) bool { return false }), failure)
	require.ErrorIs(t, c.Upsert(t.Context(), "shop/one"), failure)
	require.ErrorIs(t, c.Delete(t.Context(), "shop/one"), failure)
	resource.before = nil
	require.NoError(t, c.Prune(t.Context(), "", func(string) bool { return false }))
	require.Empty(t, resource.objects)
}

func TestCRDClient_PruneRefusesMalformedOwnership(t *testing.T) {
	c, resource := newTestCRDClient()
	require.NoError(t, c.Upsert(t.Context(), "shop/one"))
	resource.objects[crdEntryName("shop/one")].SetAnnotations(nil)
	require.ErrorContains(t, c.Prune(t.Context(), "", func(string) bool { return false }), "not an owned")
	require.Len(t, resource.objects, 1)
}
