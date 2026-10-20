// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package spire

import (
	"context"
	"crypto/sha256"
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
	listOptions   []metav1.ListOptions
	deleteOptions []metav1.DeleteOptions
	version       int
}

func newTestCRDClient() (*CRDClient, *mockCRDResource) {
	resource := &mockCRDResource{objects: map[string]*unstructured.Unstructured{}}
	return &CRDClient{
		client:      resource,
		trustDomain: "test.cilium",
		entryCfg:    DefaultSpireEntryConfig(),
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
	m.listOptions = append(m.listOptions, opts)
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
	ids := []string{
		"shop/checkout", "shop/catalog", "other/checkout", "shop-west/checkout.v2-prod",
		"shop-west/checkout.v2-prod-" + strings.Repeat("s", 236),
		strings.Repeat("n", 63) + "/" + strings.Repeat("s.", 126) + "s",
	}
	var entries []*types.Entry
	direct := &EntryClient{
		cfg:      ClientConfig{SpiffeTrustDomain: c.trustDomain},
		entryCfg: DefaultSpireEntryConfig(),
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
		namespace, _, _ := strings.Cut(id, "/")
		require.Equal(t, "ClusterStaticEntry", obj.GetKind())
		require.Equal(t, "spire.spiffe.io/v1alpha1", obj.GetAPIVersion())
		require.Empty(t, obj.GetOwnerReferences())
		require.Equal(t, map[string]string{
			"app.kubernetes.io/managed-by": "cilium-operator-ztunnel",
			"spire.cilium.io/namespace":    namespace,
		}, obj.GetLabels())
		require.Equal(t, map[string]string{crdIDAnnotation: id}, obj.GetAnnotations())
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
	require.Len(t, resource.calls, len(ids)+1)
	for _, call := range resource.calls {
		require.True(t, strings.HasPrefix(call, "get:"), call)
	}
}

func TestCRDEntryNamesAndInvalidIDs(t *testing.T) {
	names := map[string]bool{}
	for _, tt := range []struct {
		id   string
		name string
	}{
		{"shop/checkout", "ztunnel-shop.checkout"},
		{"a-b/c", "ztunnel-a-b.c"},
		{"a/b-c", "ztunnel-a.b-c"},
		{"a/b.c", "ztunnel-a.b.c"},
		{"shop-west/checkout.v2-prod", "ztunnel-shop-west.checkout.v2-prod"},
		{strings.Repeat("n", 63) + "/s", "ztunnel-" + strings.Repeat("n", 63) + ".s"},
		{strings.Repeat("n", 63) + "/" + strings.Repeat("s", 181), "ztunnel-" + strings.Repeat("n", 63) + "." + strings.Repeat("s", 181)},
		{"n/" + strings.Repeat("s", 243), "ztunnel-n." + strings.Repeat("s", 243)},
	} {
		name := crdEntryName(tt.id)
		require.Equal(t, tt.name, name)
		require.LessOrEqual(t, len(name), 253)
		require.Empty(t, validation.IsDNS1123Subdomain(name))
		require.False(t, names[name], tt.id)
		names[name] = true
	}

	c, resource := newTestCRDClient()
	for _, tt := range []struct {
		name     string
		id       string
		readable string
	}{
		{
			name:     "254 bytes with maximum namespace",
			id:       strings.Repeat("n", 63) + "/" + strings.Repeat("s", 182),
			readable: strings.Repeat("n", 63) + "-" + strings.Repeat("s", 111),
		},
		{
			name:     "254 bytes with minimum namespace",
			id:       "n/" + strings.Repeat("s", 244),
			readable: "n-" + strings.Repeat("s", 173),
		},
		{
			name:     "maximum namespace and account",
			id:       strings.Repeat("n", 63) + "/" + strings.Repeat("s", 253),
			readable: strings.Repeat("n", 63) + "-" + strings.Repeat("s", 111),
		},
		{
			name:     "different identity with same visible prefix",
			id:       strings.Repeat("n", 63) + "/" + strings.Repeat("s", 252) + "t",
			readable: strings.Repeat("n", 63) + "-" + strings.Repeat("s", 111),
		},
		{
			name:     "maximum dotted account",
			id:       strings.Repeat("n", 63) + "/" + strings.Repeat("s.", 126) + "s",
			readable: strings.Repeat("n", 63) + "-" + strings.Repeat("s-", 55) + "s",
		},
		{
			name:     "maximum hyphenated account with same visible prefix",
			id:       strings.Repeat("n", 63) + "/" + strings.Repeat("s-", 126) + "s",
			readable: strings.Repeat("n", 63) + "-" + strings.Repeat("s-", 55) + "s",
		},
		{
			name:     "readable namespace and account",
			id:       "shop-west/checkout.v2-prod-" + strings.Repeat("s", 236),
			readable: "shop-west-checkout-v2-prod-" + strings.Repeat("s", 148),
		},
		{
			name:     "dot at truncation",
			id:       "n/" + strings.Repeat("s", 172) + "." + strings.Repeat("s", 80),
			readable: "n-" + strings.Repeat("s", 172),
		},
		{
			name:     "hyphen at truncation",
			id:       "n/" + strings.Repeat("s", 172) + "-" + strings.Repeat("s", 80),
			readable: "n-" + strings.Repeat("s", 172),
		},
		{
			name:     "hyphen run at truncation",
			id:       "n/" + strings.Repeat("s", 170) + "---" + strings.Repeat("s", 80),
			readable: "n-" + strings.Repeat("s", 170),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			name := crdEntryName(tt.id)
			hash := fmt.Sprintf("%x", sha256.Sum256([]byte(tt.id)))
			require.Equal(t, "ztunnel-hash-"+tt.readable+"-"+hash, name)
			require.Equal(t, name, crdEntryName(tt.id))
			require.LessOrEqual(t, len(name), 253)
			require.NotContains(t, name, ".")
			require.Empty(t, validation.IsDNS1123Subdomain(name))
			require.False(t, names[name], tt.id)
			names[name] = true
			require.NoError(t, c.Upsert(t.Context(), tt.id))
			obj := resource.objects[name]
			require.Equal(t, tt.id, obj.GetAnnotations()[crdIDAnnotation])
			namespace, account, _ := strings.Cut(tt.id, "/")
			require.Equal(t, namespace, obj.GetLabels()[crdNamespaceLabel])
			spec, found, err := unstructured.NestedMap(obj.Object, "spec")
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, "spiffe://test.cilium/ns/"+namespace+"/sa/"+account, spec["spiffeID"])
			require.Equal(t, "spiffe://test.cilium/ztunnel", spec["parentID"])
			require.Equal(t, []any{"k8s:ns:" + namespace, "k8s:sa:" + account}, spec["selectors"])
		})
	}
	resource.calls = nil
	for _, id := range []string{"", "shop", "shop/", "/sa", "a/b/c", "Upper/sa", "ns/Upper", "shop.prod/sa", "ns/.sa", "ns/sa.", "ns/a..b", "ns/sa_name", strings.Repeat("n", 64) + "/sa", "ns/" + strings.Repeat("s", 254)} {
		require.Error(t, c.Upsert(t.Context(), id), id)
		require.Error(t, c.Delete(t.Context(), id), id)
	}
	require.NoError(t, c.UpsertBatch(t.Context(), nil))
	require.NoError(t, c.DeleteBatch(t.Context(), nil))
	require.Error(t, c.Prune(t.Context(), "bad/namespace", func(string) bool { return false }))
	require.Error(t, c.Prune(t.Context(), "", nil))
	require.Empty(t, resource.calls)
}

func TestCRDEntryNameFallbackDoesNotCollideWithOrdinaryNames(t *testing.T) {
	fallback := crdEntryName("shop/" + strings.Repeat("s", 253))
	require.Regexp(t, "^ztunnel-hash-shop-s+-[a-f0-9]{64}$", fallback)
	require.Len(t, fallback, 253)
	suffix := strings.TrimPrefix(fallback, "ztunnel-hash-")
	ordinaryID := "hash/" + suffix
	_, err := serviceAccountNamespace(ordinaryID)
	require.NoError(t, err)
	require.Equal(t, "ztunnel-hash."+suffix, crdEntryName(ordinaryID))
	require.NotEqual(t, fallback, crdEntryName(ordinaryID))
	require.NotContains(t, fallback, ".")
}

func TestCRDClient_HashNameMigrationIsExplicit(t *testing.T) {
	oversizedID := "shop/" + strings.Repeat("s", 253)
	for _, tt := range []struct {
		name       string
		id         string
		legacyName string
	}{
		{
			name:       "hashed ordinary name",
			id:         "shop/checkout",
			legacyName: "ztunnel-shop-checkout-1dfc2ea56f23fbee33054f9c71acbee4",
		},
		{
			name:       "hash-only oversized name",
			id:         oversizedID,
			legacyName: fmt.Sprintf("ztunnel-hash-%x", sha256.Sum256([]byte(oversizedID))),
		},
	} {
		for _, managedBy := range []string{crdManagedByValue, "cilium-operator"} {
			t.Run(tt.name+"/"+managedBy, func(t *testing.T) {
				c, resource := newTestCRDClient()
				name := crdEntryName(tt.id)
				require.NotEqual(t, tt.legacyName, name)
				legacy, err := c.buildClusterStaticEntry(tt.id)
				require.NoError(t, err)
				legacy.SetName(tt.legacyName)
				legacy.SetLabels(map[string]string{crdManagedByLabel: managedBy})
				legacy, err = resource.Create(t.Context(), legacy, metav1.CreateOptions{})
				require.NoError(t, err)
				resource.calls = nil

				require.NoError(t, c.Upsert(t.Context(), tt.id))
				require.Equal(t, []string{"get:" + name, "create:" + name}, resource.calls)
				require.Len(t, resource.objects, 2)
				require.Equal(t, legacy, resource.objects[legacy.GetName()])
				namespace, err := serviceAccountNamespace(tt.id)
				require.NoError(t, err)
				require.NoError(t, c.Prune(t.Context(), namespace, func(currentID string) bool { return currentID == tt.id }))
				require.Len(t, resource.objects, 2)
				require.NoError(t, c.Delete(t.Context(), tt.id))
				require.Len(t, resource.objects, 1)
				require.Equal(t, legacy, resource.objects[legacy.GetName()])
			})
		}
	}
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

func TestCRDClient_NamespaceLabelRecovery(t *testing.T) {
	for _, operation := range []string{"upsert", "global-prune"} {
		for _, label := range []string{"missing", "empty", "wrong", "correct"} {
			t.Run(operation+"/"+label, func(t *testing.T) {
				c, resource := newTestCRDClient()
				id := "shop/checkout"
				require.NoError(t, c.Upsert(t.Context(), id))
				obj := resource.objects[crdEntryName(id)]
				if operation == "global-prune" {
					delete(resource.objects, obj.GetName())
					obj.SetName("custom-registration-name")
					resource.objects[obj.GetName()] = obj
					require.NoError(t, unstructured.SetNestedField(obj.Object, "custom-parent", "spec", "parentID"))
				}
				name := obj.GetName()
				obj.SetFinalizers([]string{"spire.spiffe.io/entry"})
				objLabels := obj.GetLabels()
				objLabels["other.example/label"] = "keep"
				switch label {
				case "missing":
					delete(objLabels, crdNamespaceLabel)
				case "empty":
					objLabels[crdNamespaceLabel] = ""
				case "wrong":
					objLabels[crdNamespaceLabel] = "other"
				}
				obj.SetLabels(objLabels)
				obj.SetAnnotations(map[string]string{
					crdIDAnnotation:            id,
					"other.example/annotation": "keep",
				})
				require.NoError(t, unstructured.SetNestedField(obj.Object, "spire", "spec", "className"))
				obj.Object["status"] = map[string]any{"entryID": "opaque-id"}
				before := obj.DeepCopy()
				reconcile := func() error {
					if operation == "upsert" {
						return c.Upsert(t.Context(), id)
					}
					return c.Prune(t.Context(), "", func(currentID string) bool {
						require.Equal(t, id, currentID)
						return true
					})
				}
				firstCall := "get:" + name
				if operation == "global-prune" {
					firstCall = "list:"
				}
				resource.calls = nil
				require.NoError(t, reconcile())
				expectedCalls := []string{firstCall}
				if label != "correct" {
					expectedCalls = append(expectedCalls, "update:"+name)
				}
				require.Equal(t, expectedCalls, resource.calls)
				updated := resource.objects[name].DeepCopy()
				expected := before.DeepCopy()
				require.NoError(t, unstructured.SetNestedField(expected.Object, "shop", "metadata", "labels", crdNamespaceLabel))
				expected.SetResourceVersion(updated.GetResourceVersion())
				require.Equal(t, expected, updated)

				resource.calls = nil
				require.NoError(t, reconcile())
				require.Equal(t, []string{firstCall}, resource.calls)
				require.Equal(t, updated, resource.objects[name])

				resource.listOptions = nil
				require.NoError(t, c.Prune(t.Context(), "shop", func(currentID string) bool {
					require.Equal(t, id, currentID)
					return false
				}))
				require.Equal(t, []metav1.ListOptions{{
					LabelSelector: "app.kubernetes.io/managed-by=cilium-operator-ztunnel,spire.cilium.io/namespace=shop",
					Limit:         500,
				}}, resource.listOptions)
				require.Empty(t, resource.objects)
				require.Len(t, resource.deleteOptions, 1)
				require.Equal(t, updated.GetUID(), *resource.deleteOptions[0].Preconditions.UID)
				require.Equal(t, updated.GetResourceVersion(), *resource.deleteOptions[0].Preconditions.ResourceVersion)
			})
		}
	}
}

func TestCRDClient_NamespaceLabelConflicts(t *testing.T) {
	for _, operation := range []string{"upsert", "global-prune"} {
		for _, change := range []string{"ownership", "identity", "recreation"} {
			t.Run(operation+"/"+change, func(t *testing.T) {
				c, resource := newTestCRDClient()
				id, name := "shop/checkout", crdEntryName("shop/checkout")
				require.NoError(t, c.Upsert(t.Context(), id))
				obj := resource.objects[name]
				obj.SetLabels(map[string]string{crdManagedByLabel: crdManagedByValue})
				var raced *unstructured.Unstructured
				resource.before = func(verb, _ string) error {
					if verb == "update" {
						switch change {
						case "ownership":
							obj.SetLabels(map[string]string{crdManagedByLabel: "someone-else"})
						case "identity":
							obj.SetAnnotations(map[string]string{crdIDAnnotation: "other/checkout"})
						case "recreation":
							obj.SetUID("new-uid")
						}
						obj.SetResourceVersion("999")
						raced = obj.DeepCopy()
					}
					return nil
				}
				var err error
				if operation == "upsert" {
					err = c.Upsert(t.Context(), id)
				} else {
					err = c.Prune(t.Context(), "", func(string) bool { return true })
				}
				require.True(t, apierrors.IsConflict(err), "%v", err)
				require.NotNil(t, raced)
				require.Equal(t, raced, resource.objects[name])
				require.Empty(t, resource.deleteOptions)
			})
		}
	}
}

func TestCRDClient_GlobalPruneNamespaceLabelErrors(t *testing.T) {
	c, resource := newTestCRDClient()
	ids := []string{"shop/one", "shop/two", "shop/invalid"}
	require.NoError(t, c.UpsertBatch(t.Context(), ids))
	for _, obj := range resource.objects {
		obj.SetLabels(map[string]string{crdManagedByLabel: crdManagedByValue})
	}
	invalid := resource.objects[crdEntryName("shop/invalid")]
	invalid.SetAnnotations(nil)
	invalidBefore := invalid.DeepCopy()
	failedBefore := resource.objects[crdEntryName("shop/one")].DeepCopy()
	failure := errors.New("namespace label update failed")
	resource.before = func(verb, name string) error {
		if verb == "update" && name == failedBefore.GetName() {
			return failure
		}
		return nil
	}
	keep := func(id string) bool {
		require.Contains(t, ids, id)
		return true
	}
	err := c.Prune(t.Context(), "", keep)
	require.ErrorIs(t, err, failure)
	require.ErrorContains(t, err, `update namespace label for ServiceAccount "shop/one"`)
	require.ErrorContains(t, err, `invalid ServiceAccount ID ""`)
	require.Equal(t, failedBefore, resource.objects[failedBefore.GetName()])
	require.Equal(t, invalidBefore, resource.objects[invalid.GetName()])
	require.Equal(t, "shop", resource.objects[crdEntryName("shop/two")].GetLabels()[crdNamespaceLabel])
	require.Empty(t, resource.deleteOptions)

	resource.before = nil
	require.ErrorContains(t, c.Prune(t.Context(), "", keep), `invalid ServiceAccount ID ""`)
	require.Equal(t, "shop", resource.objects[crdEntryName("shop/one")].GetLabels()[crdNamespaceLabel])
	require.Equal(t, invalidBefore, resource.objects[invalid.GetName()])
	invalid.SetAnnotations(map[string]string{crdIDAnnotation: "shop/invalid"})
	require.NoError(t, c.Prune(t.Context(), "", keep))
	require.Equal(t, "shop", resource.objects[invalid.GetName()].GetLabels()[crdNamespaceLabel])
}

func TestCRDClient_GlobalPruneNamespaceLabelPagination(t *testing.T) {
	c, resource := newTestCRDClient()
	ids := []string{"shop/one", "other/two"}
	require.NoError(t, c.UpsertBatch(t.Context(), ids))
	for _, obj := range resource.objects {
		obj.SetLabels(map[string]string{crdManagedByLabel: crdManagedByValue})
	}
	pages := 0
	resource.list = func(opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
		require.Equal(t, "app.kubernetes.io/managed-by=cilium-operator-ztunnel", opts.LabelSelector)
		require.EqualValues(t, 500, opts.Limit)
		list := &unstructured.UnstructuredList{}
		if pages == 0 {
			require.Empty(t, opts.Continue)
			list.Items = []unstructured.Unstructured{*resource.objects[crdEntryName(ids[0])].DeepCopy()}
			list.SetContinue("second-page")
		} else {
			require.Equal(t, "second-page", opts.Continue)
			list.Items = []unstructured.Unstructured{*resource.objects[crdEntryName(ids[1])].DeepCopy()}
		}
		pages++
		return list, nil
	}
	keepSecond := true
	resource.before = func(verb, _ string) error {
		if verb == "update" {
			keepSecond = false
		}
		return nil
	}
	var checked []string
	require.NoError(t, c.Prune(t.Context(), "", func(id string) bool {
		checked = append(checked, id)
		return id == ids[0] || keepSecond
	}))
	require.Equal(t, ids, checked)
	require.Equal(t, 2, pages)
	require.Len(t, resource.objects, 1)
	require.Equal(t, "shop", resource.objects[crdEntryName(ids[0])].GetLabels()[crdNamespaceLabel])
}

func TestCRDClient_GlobalPruneTerminatingNamespaceLabel(t *testing.T) {
	c, resource := newTestCRDClient()
	id := "shop/checkout"
	require.NoError(t, c.Upsert(t.Context(), id))
	obj := resource.objects[crdEntryName(id)]
	obj.SetLabels(map[string]string{crdManagedByLabel: crdManagedByValue})
	now := metav1.Now()
	obj.SetDeletionTimestamp(&now)
	before := obj.DeepCopy()
	resource.calls = nil
	require.NoError(t, c.Prune(t.Context(), "", func(string) bool { return true }))
	require.Equal(t, []string{"list:"}, resource.calls)
	require.Equal(t, before, resource.objects[obj.GetName()])
}

func TestCRDClient_SingleLabelOwnership(t *testing.T) {
	for _, legacyLabels := range []bool{false, true} {
		t.Run(fmt.Sprint(legacyLabels), func(t *testing.T) {
			c, resource := newTestCRDClient()
			id := "shop/checkout"
			require.NoError(t, c.Upsert(t.Context(), id))
			obj := resource.objects[crdEntryName(id)]
			labels := map[string]string{crdManagedByLabel: "cilium-operator-ztunnel"}
			if legacyLabels {
				labels["spire.cilium.io/registration"] = "another-purpose"
				labels[crdNamespaceLabel] = "another-namespace"
			}
			obj.SetLabels(labels)
			require.NoError(t, unstructured.SetNestedField(obj.Object, "old", "spec", "parentID"))
			require.NoError(t, c.Upsert(t.Context(), id))
			expectedLabels := maps.Clone(labels)
			expectedLabels[crdNamespaceLabel] = "shop"
			require.Equal(t, expectedLabels, resource.objects[obj.GetName()].GetLabels())
			resource.objects[obj.GetName()].SetLabels(labels)
			require.NoError(t, c.Delete(t.Context(), id))
			require.Empty(t, resource.objects)
		})
	}
}

func TestCRDClient_RefusesForeignOwnership(t *testing.T) {
	for _, tt := range []struct {
		name   string
		labels map[string]string
	}{
		{name: "missing"},
		{name: "empty", labels: map[string]string{crdManagedByLabel: ""}},
		{name: "foreign", labels: map[string]string{crdManagedByLabel: "another-operator"}},
		{name: "foreign with namespace", labels: map[string]string{
			crdManagedByLabel: "another-operator",
			crdNamespaceLabel: "shop",
		}},
		{name: "namespace label only", labels: map[string]string{crdNamespaceLabel: "shop"}},
		{name: "legacy", labels: map[string]string{
			crdManagedByLabel:              "cilium-operator",
			"spire.cilium.io/registration": "ztunnel-serviceaccount",
			"spire.cilium.io/namespace":    "shop",
		}},
		{name: "legacy metadata only", labels: map[string]string{
			"spire.cilium.io/registration": "ztunnel-serviceaccount",
			"spire.cilium.io/namespace":    "shop",
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, resource := newTestCRDClient()
			id := "shop/checkout"
			require.NoError(t, c.Upsert(t.Context(), id))
			obj := resource.objects[crdEntryName(id)]
			obj.SetLabels(tt.labels)
			before := obj.DeepCopy()
			require.ErrorContains(t, c.Upsert(t.Context(), id), "not an owned")
			require.ErrorContains(t, c.Delete(t.Context(), id), "not an owned")
			for _, namespace := range []string{"", "shop"} {
				require.NoError(t, c.Prune(t.Context(), namespace, func(string) bool {
					t.Fatal("foreign registration reached the desired-state inventory")
					return false
				}))
			}
			require.Equal(t, before, resource.objects[obj.GetName()])
			require.Empty(t, resource.deleteOptions)
		})
	}
}

func TestCRDClient_RefusesMismatchedIdentity(t *testing.T) {
	for _, otherID := range []string{"shop/catalog", "other/checkout"} {
		t.Run(otherID, func(t *testing.T) {
			c, resource := newTestCRDClient()
			id := "shop/checkout"
			require.NoError(t, c.Upsert(t.Context(), id))
			obj := resource.objects[crdEntryName(id)]
			obj.SetAnnotations(map[string]string{crdIDAnnotation: otherID})
			before := obj.DeepCopy()
			wantErr := fmt.Sprintf("belongs to ServiceAccount %q, not %q", otherID, id)
			require.ErrorContains(t, c.Upsert(t.Context(), id), wantErr)
			require.ErrorContains(t, c.Delete(t.Context(), id), wantErr)
			require.Equal(t, before, resource.objects[obj.GetName()])
			require.Empty(t, resource.deleteOptions)
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

func TestCRDClient_ConcurrentIdentityChange(t *testing.T) {
	for _, verb := range []string{"update", "delete", "prune"} {
		t.Run(verb, func(t *testing.T) {
			c, resource := newTestCRDClient()
			id, name := "shop/checkout", crdEntryName("shop/checkout")
			require.NoError(t, c.Upsert(t.Context(), id))
			obj := resource.objects[name]
			require.NoError(t, unstructured.SetNestedField(obj.Object, "old", "spec", "parentID"))
			resource.before = func(operation, _ string) error {
				if operation == verb || verb == "prune" && operation == "delete" {
					obj.SetAnnotations(map[string]string{crdIDAnnotation: "other/checkout"})
					obj.SetResourceVersion("999")
				}
				return nil
			}
			var err error
			switch verb {
			case "update":
				err = c.Upsert(t.Context(), id)
			case "delete":
				err = c.Delete(t.Context(), id)
			case "prune":
				err = c.Prune(t.Context(), "shop", func(string) bool { return false })
			}
			require.True(t, apierrors.IsConflict(err), "%v", err)
			require.Equal(t, obj, resource.objects[name])
			require.Equal(t, "other/checkout", resource.objects[name].GetAnnotations()[crdIDAnnotation])
			resource.before = nil
			require.ErrorContains(t, c.Upsert(t.Context(), id), "belongs to ServiceAccount")
			require.ErrorContains(t, c.Delete(t.Context(), id), "belongs to ServiceAccount")
		})
	}
}

func TestCRDClient_PruneOwnershipAndScope(t *testing.T) {
	c, resource := newTestCRDClient()
	require.NoError(t, c.UpsertBatch(t.Context(), []string{"shop/keep", "shop/scoped-stale", "shop/stale", "shop/legacy", "other/stale", "shopping/stale"}))
	scopedStale := resource.objects[crdEntryName("shop/scoped-stale")].DeepCopy()
	legacy := resource.objects[crdEntryName("shop/legacy")]
	legacy.SetLabels(map[string]string{crdManagedByLabel: crdManagedByValue})
	stale := resource.objects[crdEntryName("shop/stale")]
	delete(resource.objects, stale.GetName())
	stale.SetName("custom-registration-name")
	stale.SetLabels(map[string]string{
		crdManagedByLabel:              crdManagedByValue,
		"spire.cilium.io/registration": "another-purpose",
		crdNamespaceLabel:              "other",
	})
	resource.objects[stale.GetName()] = stale
	resource.objects[crdEntryName("other/stale")].SetLabels(map[string]string{
		crdManagedByLabel: crdManagedByValue,
		crdNamespaceLabel: "shop",
	})
	for _, name := range []string{"ztunnel", "spire-agent-node-alias", "old-poc-entry"} {
		obj := &unstructured.Unstructured{}
		obj.SetName(name)
		obj.SetLabels(map[string]string{crdManagedByLabel: "cilium-operator"})
		resource.objects[name] = obj
	}
	var checked []string
	require.NoError(t, c.Prune(t.Context(), "shop", func(id string) bool {
		checked = append(checked, id)
		return id == "shop/keep"
	}))
	require.ElementsMatch(t, []string{"shop/keep", "shop/scoped-stale"}, checked)
	require.Equal(t, []metav1.ListOptions{{
		LabelSelector: "app.kubernetes.io/managed-by=cilium-operator-ztunnel,spire.cilium.io/namespace=shop",
		Limit:         500,
	}}, resource.listOptions)
	require.NotContains(t, resource.objects, scopedStale.GetName())
	require.Equal(t, scopedStale.GetUID(), *resource.deleteOptions[0].Preconditions.UID)
	require.Equal(t, scopedStale.GetResourceVersion(), *resource.deleteOptions[0].Preconditions.ResourceVersion)
	require.Contains(t, resource.objects, stale.GetName())
	require.Contains(t, resource.objects, legacy.GetName())
	require.Contains(t, resource.objects, crdEntryName("other/stale"))
	require.Contains(t, resource.objects, crdEntryName("shopping/stale"))
	var expectedPreconditions []metav1.Preconditions
	for _, id := range []string{"shop/legacy", "other/stale", "shopping/stale"} {
		obj := resource.objects[crdEntryName(id)]
		uid, rv := obj.GetUID(), obj.GetResourceVersion()
		expectedPreconditions = append(expectedPreconditions, metav1.Preconditions{UID: &uid, ResourceVersion: &rv})
	}
	uid, rv := stale.GetUID(), stale.GetResourceVersion()
	expectedPreconditions = append(expectedPreconditions, metav1.Preconditions{UID: &uid, ResourceVersion: &rv})
	require.NoError(t, c.Prune(t.Context(), "", func(id string) bool { return id == "shop/keep" }))
	require.Equal(t, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=cilium-operator-ztunnel",
		Limit:         500,
	}, resource.listOptions[1])
	var actualPreconditions []metav1.Preconditions
	for _, opts := range resource.deleteOptions[1:] {
		actualPreconditions = append(actualPreconditions, *opts.Preconditions)
	}
	require.ElementsMatch(t, expectedPreconditions, actualPreconditions)
	require.Len(t, resource.objects, 4)
	require.Contains(t, resource.objects, crdEntryName("shop/keep"))
}

func TestCRDClient_PrunePaginationAndFailures(t *testing.T) {
	c, resource := newTestCRDClient()
	require.NoError(t, c.UpsertBatch(t.Context(), []string{"shop/one", "shop/two"}))
	pages := 0
	resource.list = func(opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
		require.EqualValues(t, 500, opts.Limit)
		require.Equal(t, "app.kubernetes.io/managed-by=cilium-operator-ztunnel,spire.cilium.io/namespace=shop", opts.LabelSelector)
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
	require.ErrorIs(t, c.Prune(t.Context(), "shop", func(string) bool { return false }), failure)
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

func TestCRDClient_RefusesMalformedIdentity(t *testing.T) {
	for _, namespace := range []string{"", "shop"} {
		for _, id := range []string{"", "shop", "shop/", "/one", "shop/one/extra", "Shop/one", "shop/One"} {
			t.Run(namespace+"/"+id, func(t *testing.T) {
				c, resource := newTestCRDClient()
				require.NoError(t, c.UpsertBatch(t.Context(), []string{"shop/one", "shop/stale"}))
				obj := resource.objects[crdEntryName("shop/one")]
				if id == "" {
					obj.SetAnnotations(nil)
				} else {
					obj.SetAnnotations(map[string]string{crdIDAnnotation: id})
				}
				before := obj.DeepCopy()
				wantErr := fmt.Sprintf("ClusterStaticEntry %q: invalid ServiceAccount ID", obj.GetName())
				require.ErrorContains(t, c.Upsert(t.Context(), "shop/one"), wantErr)
				require.ErrorContains(t, c.Delete(t.Context(), "shop/one"), wantErr)
				var checked []string
				require.ErrorContains(t, c.Prune(t.Context(), namespace, func(id string) bool {
					checked = append(checked, id)
					return false
				}), wantErr)
				require.Equal(t, []string{"shop/stale"}, checked)
				require.Len(t, resource.objects, 1)
				require.Equal(t, before, resource.objects[obj.GetName()])
			})
		}
	}
}
