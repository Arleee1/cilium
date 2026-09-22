// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package spire

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/cilium/cilium/pkg/logging/logfields"
)

// POC: an alternative registration backend that writes spire-controller-manager
// ClusterStaticEntry custom resources to the Kubernetes API server instead of
// calling the SPIRE server Entry API directly. A per-server spire-controller-manager
// then reconciles these CRs into each server's datastore, so the API server becomes
// the shared source of truth (no shared external database required).

// clusterStaticEntryGVR is the GroupVersionResource for the spire-controller-manager
// ClusterStaticEntry CRD (spire.spiffe.io/v1alpha1).
var clusterStaticEntryGVR = schema.GroupVersionResource{
	Group:    "spire.spiffe.io",
	Version:  "v1alpha1",
	Resource: "clusterstaticentries",
}

// crdEntryName derives a deterministic, DNS-safe ClusterStaticEntry name from a
// workload id of the form "namespace/serviceaccount".
func crdEntryName(id string) string {
	return "ztunnel-" + strings.ToLower(strings.ReplaceAll(id, "/", "-"))
}

// buildClusterStaticEntry builds the unstructured ClusterStaticEntry for the given id
// using the same SpireEntryConfig (SPIFFE ID path, parent ID, selectors) the direct
// gRPC path uses, so the resulting entry is identical.
func (c *Client) buildClusterStaticEntry(id string) *unstructured.Unstructured {
	spiffeID := fmt.Sprintf("spiffe://%s%s", c.cfg.SpiffeTrustDomain, c.entryCfg.PathFunc(id))
	parentID := fmt.Sprintf("spiffe://%s%s", c.cfg.SpiffeTrustDomain, c.entryCfg.ParentID)

	selectors := make([]any, 0)
	for _, s := range c.entryCfg.SelectorsFunc(id) {
		selectors = append(selectors, fmt.Sprintf("%s:%s", s.Type, s.Value))
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "spire.spiffe.io",
		Version: "v1alpha1",
		Kind:    "ClusterStaticEntry",
	})
	obj.SetName(crdEntryName(id))
	obj.Object["spec"] = map[string]any{
		"spiffeID":  spiffeID,
		"parentID":  parentID,
		"selectors": selectors,
	}
	return obj
}

// upsertCRD creates or updates the ClusterStaticEntry for a single id.
func (c *Client) upsertCRD(ctx context.Context, id string) error {
	if c.dynamicClient == nil {
		return fmt.Errorf("dynamic client not initialized")
	}
	desired := c.buildClusterStaticEntry(id)
	res := c.dynamicClient.Resource(clusterStaticEntryGVR)

	existing, err := res.Get(ctx, desired.GetName(), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			_, err = res.Create(ctx, desired, metav1.CreateOptions{})
			return err
		}
		return err
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	_, err = res.Update(ctx, desired, metav1.UpdateOptions{})
	return err
}

// upsertCRDBatch upserts ClusterStaticEntries for all ids.
func (c *Client) upsertCRDBatch(ctx context.Context, ids []string) error {
	var errs []error
	for _, id := range ids {
		if err := c.upsertCRD(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("upsert CRD for %s: %w", id, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("upsertCRDBatch had %d errors: %w", len(errs), errs[0])
	}
	c.log.Debug("upsertCRDBatch completed", logfields.Count, len(ids))
	return nil
}

// deleteCRD deletes the ClusterStaticEntry for a single id (NotFound is ignored).
func (c *Client) deleteCRD(ctx context.Context, id string) error {
	if c.dynamicClient == nil {
		return fmt.Errorf("dynamic client not initialized")
	}
	err := c.dynamicClient.Resource(clusterStaticEntryGVR).Delete(ctx, crdEntryName(id), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// deleteCRDBatch deletes ClusterStaticEntries for all ids.
func (c *Client) deleteCRDBatch(ctx context.Context, ids []string) error {
	var errs []error
	for _, id := range ids {
		if err := c.deleteCRD(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("delete CRD for %s: %w", id, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("deleteCRDBatch had %d errors: %w", len(errs), errs[0])
	}
	c.log.Debug("deleteCRDBatch completed", logfields.Count, len(ids))
	return nil
}
