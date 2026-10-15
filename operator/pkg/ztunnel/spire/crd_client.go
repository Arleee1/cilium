// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package spire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/cilium/hive/cell"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

var clusterStaticEntryGVR = schema.GroupVersionResource{
	Group:    "spire.spiffe.io",
	Version:  "v1alpha1",
	Resource: "clusterstaticentries",
}

const (
	crdManagedByLabel = "app.kubernetes.io/managed-by"
	crdManagedByValue = "cilium-operator-ztunnel"
	crdIDAnnotation   = "spire.cilium.io/service-account"
	maxCRDNameLen     = 63
)

// CRDClient persists one ClusterStaticEntry per ServiceAccount. Each SPIRE
// controller-manager independently reconciles these resources into its server.
// CR names are not SPIRE entry IDs or logical ServiceAccount identities.
type CRDClient struct {
	client      dynamic.ResourceInterface
	trustDomain string
	entryCfg    SpireEntryConfig
	initialized chan struct{}
}

var _ SpireClient = (*CRDClient)(nil)
var _ ServiceAccountPruner = (*CRDClient)(nil)

func newCRDClient(params params) (*CRDClient, error) {
	if params.K8sClient == nil || !params.K8sClient.IsEnabled() || params.K8sClient.RestConfig() == nil {
		return nil, fmt.Errorf("SPIRE CRD backend requires a Kubernetes client")
	}
	if _, err := spiffeid.TrustDomainFromString(params.ClientConfig.SpiffeTrustDomain); err != nil {
		return nil, fmt.Errorf("invalid SPIRE CRD trust domain: %w", err)
	}
	dynClient, err := dynamic.NewForConfig(params.K8sClient.RestConfig())
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client for SPIRE CRD backend: %w", err)
	}
	client := &CRDClient{
		client:      dynClient.Resource(clusterStaticEntryGVR),
		trustDomain: params.ClientConfig.SpiffeTrustDomain,
		entryCfg:    params.EntryConfig,
		initialized: make(chan struct{}),
	}
	params.Lifecycle.Append(cell.Hook{OnStart: client.onStart})
	return client, nil
}

func (c *CRDClient) onStart(cell.HookContext) error {
	close(c.initialized)
	return nil
}

func (c *CRDClient) Initialized() <-chan struct{} {
	return c.initialized
}

func serviceAccountNamespace(id string) (string, error) {
	namespace, account, ok := strings.Cut(id, "/")
	if !ok || len(validation.IsDNS1123Label(namespace)) != 0 ||
		len(validation.IsDNS1123Subdomain(account)) != 0 {
		return "", fmt.Errorf("invalid ServiceAccount ID %q: expected namespace/serviceaccount", id)
	}
	return namespace, nil
}

func crdEntryName(id string) string {
	sum := sha256.Sum256([]byte(id))
	suffix := "-" + hex.EncodeToString(sum[:16])
	name := "ztunnel-" + strings.NewReplacer("/", "-", ".", "-").Replace(id)
	if len(name) > maxCRDNameLen-len(suffix) {
		name = name[:maxCRDNameLen-len(suffix)]
	}
	return strings.TrimRight(name, "-") + suffix
}

func (c *CRDClient) buildClusterStaticEntry(id string) (*unstructured.Unstructured, error) {
	if _, err := serviceAccountNamespace(id); err != nil {
		return nil, err
	}
	entry := c.entryCfg.entry(c.trustDomain, id)
	selectors := make([]any, 0, len(entry.Selectors))
	for _, selector := range entry.Selectors {
		selectors = append(selectors, selector.Type+":"+selector.Value)
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   clusterStaticEntryGVR.Group,
		Version: clusterStaticEntryGVR.Version,
		Kind:    "ClusterStaticEntry",
	})
	obj.SetName(crdEntryName(id))
	obj.SetLabels(map[string]string{
		crdManagedByLabel: crdManagedByValue,
	})
	// A namespaced ServiceAccount cannot own a cluster-scoped resource.
	obj.SetAnnotations(map[string]string{crdIDAnnotation: id})
	obj.Object["spec"] = map[string]any{
		"spiffeID":  "spiffe://" + entry.SpiffeId.TrustDomain + entry.SpiffeId.Path,
		"parentID":  "spiffe://" + entry.ParentId.TrustDomain + entry.ParentId.Path,
		"selectors": selectors,
	}
	return obj, nil
}

// ownedID checks the producer label before validating the account identity used
// to scope mutations. Identity metadata does not establish ownership.
func ownedID(obj *unstructured.Unstructured) (string, error) {
	if obj.GetLabels()[crdManagedByLabel] != crdManagedByValue {
		return "", fmt.Errorf("ClusterStaticEntry %q is not an owned ServiceAccount registration", obj.GetName())
	}
	id := obj.GetAnnotations()[crdIDAnnotation]
	if _, err := serviceAccountNamespace(id); err != nil {
		return "", fmt.Errorf("ClusterStaticEntry %q: %w", obj.GetName(), err)
	}
	return id, nil
}

func (c *CRDClient) Upsert(ctx context.Context, id string) error {
	desired, err := c.buildClusterStaticEntry(id)
	if err != nil {
		return err
	}
	existing, err := c.client.Get(ctx, desired.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = c.client.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if existingID, err := ownedID(existing); err != nil {
		return err
	} else if existingID != id {
		return fmt.Errorf("ClusterStaticEntry %q belongs to ServiceAccount %q, not %q", existing.GetName(), existingID, id)
	}
	if existing.GetDeletionTimestamp() != nil {
		return fmt.Errorf("ClusterStaticEntry %q is still being deleted", existing.GetName())
	}

	// Preserve controller fields, status, finalizers, and unrelated metadata.
	// Update uses the observed resourceVersion, so an ownership change or a
	// delete/recreate racing this write fails instead of being overwritten.
	updated := existing.DeepCopy()
	for key, value := range desired.Object["spec"].(map[string]any) {
		if err := unstructured.SetNestedField(updated.Object, value, "spec", key); err != nil {
			return err
		}
	}
	if reflect.DeepEqual(existing.Object, updated.Object) {
		return nil
	}
	_, err = c.client.Update(ctx, updated, metav1.UpdateOptions{})
	return err
}

func (c *CRDClient) UpsertBatch(ctx context.Context, ids []string) error {
	var errs []error
	for _, id := range ids {
		if err := c.Upsert(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("upsert ServiceAccount %q: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (c *CRDClient) Delete(ctx context.Context, id string) error {
	if _, err := serviceAccountNamespace(id); err != nil {
		return err
	}
	existing, err := c.client.Get(ctx, crdEntryName(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if existingID, err := ownedID(existing); err != nil {
		return err
	} else if existingID != id {
		return fmt.Errorf("ClusterStaticEntry %q belongs to ServiceAccount %q, not %q", existing.GetName(), existingID, id)
	}
	return c.deleteEntry(ctx, existing)
}

func (c *CRDClient) deleteEntry(ctx context.Context, obj *unstructured.Unstructured) error {
	uid, version := obj.GetUID(), obj.GetResourceVersion()
	err := c.client.Delete(ctx, obj.GetName(), metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version},
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (c *CRDClient) DeleteBatch(ctx context.Context, ids []string) error {
	var errs []error
	for _, id := range ids {
		if err := c.Delete(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("delete ServiceAccount %q: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (c *CRDClient) Prune(ctx context.Context, namespace string, keep func(string) bool) error {
	if namespace != "" && len(validation.IsDNS1123Label(namespace)) != 0 {
		return fmt.Errorf("invalid namespace %q", namespace)
	}
	if keep == nil {
		return fmt.Errorf("ServiceAccount pruning requires an initialized desired-state inventory")
	}
	selector := labels.Set{
		crdManagedByLabel: crdManagedByValue,
	}
	options := metav1.ListOptions{LabelSelector: selector.String(), Limit: 500}
	var errs []error
	for {
		list, err := c.client.List(ctx, options)
		if err != nil {
			return errors.Join(append(errs, fmt.Errorf("list ServiceAccount registrations: %w", err))...)
		}
		for i := range list.Items {
			obj := &list.Items[i]
			id, err := ownedID(obj)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if namespace != "" && !strings.HasPrefix(id, namespace+"/") {
				continue
			}
			if !keep(id) {
				if err := c.deleteEntry(ctx, obj); err != nil {
					errs = append(errs, fmt.Errorf("prune ServiceAccount %q: %w", id, err))
				}
			}
		}
		if list.GetContinue() == "" {
			return errors.Join(errs...)
		}
		options.Continue = list.GetContinue()
	}
}
