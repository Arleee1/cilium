// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package spire

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/yaml"
)

const (
	bootstrapManagedBy = "cilium-operator-spire-bootstrap"
	startupInterval    = 500 * time.Millisecond
	startupTimeout     = 60 * time.Second
)

//go:embed crds/spire.spiffe.io_clusterstaticentries.yaml
var clusterStaticEntrySchema []byte

type bootstrapEntry struct {
	Name string             `json:"name"`
	Spec bootstrapEntrySpec `json:"spec"`
}

type bootstrapEntrySpec struct {
	SPIFFEID  string   `json:"spiffeID"`
	ParentID  string   `json:"parentID"`
	Selectors []string `json:"selectors"`
	ClassName string   `json:"className,omitempty"`
}

func parseBootstrapEntries(cfg ClientConfig) ([]*unstructured.Unstructured, spiffeid.ID, error) {
	var entries []bootstrapEntry
	decoder := json.NewDecoder(strings.NewReader(cfg.BootstrapEntries))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entries); err != nil {
		return nil, spiffeid.ID{}, fmt.Errorf("decode mesh-auth-spire-bootstrap-entries: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, spiffeid.ID{}, fmt.Errorf("mesh-auth-spire-bootstrap-entries must contain one JSON array")
	}
	if len(entries) != 2 {
		return nil, spiffeid.ID{}, fmt.Errorf("SPIRE HA requires exactly two bootstrap entries: the agent alias and ztunnel")
	}
	var agent, ztunnel *bootstrapEntry
	var alias spiffeid.ID
	for i := range entries {
		entry := &entries[i]
		if len(validation.IsDNS1123Subdomain(entry.Name)) != 0 || entry.Spec.ClassName != cfg.CRDClassName {
			return nil, alias, fmt.Errorf("invalid bootstrap name or controller class for %q", entry.Name)
		}
		id, err := spiffeid.FromString(entry.Spec.SPIFFEID)
		if err != nil || id.TrustDomain().Name() != cfg.SpiffeTrustDomain {
			return nil, alias, fmt.Errorf("bootstrap %q must have a SPIFFE ID in trust domain %q", entry.Name, cfg.SpiffeTrustDomain)
		}
		if id.Path() == "/ztunnel" {
			ztunnel = entry
			continue
		}
		parts := strings.Split(id.Path(), "/")
		if len(parts) != 5 || parts[1] != "ns" || parts[3] != "sa" ||
			len(validation.IsDNS1123Label(parts[2])) != 0 || len(validation.IsDNS1123Subdomain(parts[4])) != 0 {
			return nil, alias, fmt.Errorf("bootstrap %q must be the SPIRE agent namespace/ServiceAccount alias", entry.Name)
		}
		expected := []string{"k8s_psat:agent_ns:" + parts[2], "k8s_psat:agent_sa:" + parts[4]}
		selectors := slices.Clone(entry.Spec.Selectors)
		if len(selectors) == 3 {
			cluster := slices.IndexFunc(selectors, func(s string) bool { return strings.HasPrefix(s, "k8s_psat:cluster:") })
			if cluster < 0 || len(validation.IsDNS1123Label(strings.TrimPrefix(selectors[cluster], "k8s_psat:cluster:"))) != 0 {
				return nil, alias, fmt.Errorf("bootstrap %q has an invalid PSAT cluster selector", entry.Name)
			}
			selectors = slices.Delete(selectors, cluster, cluster+1)
		}
		slices.Sort(selectors)
		slices.Sort(expected)
		if !slices.Equal(selectors, expected) || entry.Spec.ParentID != "spiffe://"+cfg.SpiffeTrustDomain+"/spire/server" {
			return nil, alias, fmt.Errorf("bootstrap %q must use the SPIRE server parent and exact agent PSAT selectors", entry.Name)
		}
		agent, alias = entry, id
	}
	if agent == nil || ztunnel == nil || agent.Name == ztunnel.Name {
		return nil, alias, fmt.Errorf("SPIRE HA requires distinct agent alias and ztunnel bootstrap entries")
	}
	selectors := slices.Clone(ztunnel.Spec.Selectors)
	slices.Sort(selectors)
	if ztunnel.Spec.ParentID != alias.String() || len(selectors) != 2 ||
		!strings.HasPrefix(selectors[0], "k8s:ns:") || len(validation.IsDNS1123Label(strings.TrimPrefix(selectors[0], "k8s:ns:"))) != 0 ||
		!strings.HasPrefix(selectors[1], "k8s:sa:") || len(validation.IsDNS1123Subdomain(strings.TrimPrefix(selectors[1], "k8s:sa:"))) != 0 {
		return nil, alias, fmt.Errorf("ztunnel bootstrap must use the agent alias parent and exact namespace/ServiceAccount selectors")
	}
	objects := make([]*unstructured.Unstructured, 0, 2)
	for _, entry := range []*bootstrapEntry{agent, ztunnel} {
		spec := map[string]any{
			"spiffeID": entry.Spec.SPIFFEID, "parentID": entry.Spec.ParentID,
		}
		ss := make([]any, len(entry.Spec.Selectors))
		for i, selector := range entry.Spec.Selectors {
			ss[i] = selector
		}
		spec["selectors"] = ss
		if entry.Spec.ClassName != "" {
			spec["className"] = entry.Spec.ClassName
		}
		objects = append(objects, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": clusterStaticEntryGVR.GroupVersion().String(),
			"kind":       "ClusterStaticEntry",
			"metadata": map[string]any{
				"name": entry.Name, "labels": map[string]any{crdManagedByLabel: bootstrapManagedBy},
			},
			"spec": spec,
		}})
	}
	return objects, alias, nil
}

func staticEntryCRD() (*apiextensionsv1.CustomResourceDefinition, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.UnmarshalStrict(clusterStaticEntrySchema, crd); err != nil {
		return nil, fmt.Errorf("decode embedded SPIRE CRD: %w", err)
	}
	apiextensionsv1.SetObjectDefaults_CustomResourceDefinition(crd)
	return crd, nil
}

func retryableStartup(err error) bool {
	return apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) ||
		apierrors.IsAlreadyExists(err)
}

func (c *CRDClient) startup(ctx context.Context, operation string, fn func(context.Context) (bool, error)) error {
	var lastErr error
	err := wait.PollUntilContextTimeout(ctx, startupInterval, startupTimeout, true, func(ctx context.Context) (bool, error) {
		ready, err := fn(ctx)
		if retryableStartup(err) {
			lastErr = err
			c.logger.Warn("Retrying SPIRE HA startup", "operation", operation, "error", err)
			return false, nil
		}
		return ready, err
	})
	if err != nil {
		return errors.Join(err, lastErr)
	}
	return nil
}

func (c *CRDClient) ensureCRD(ctx context.Context) error {
	desired, err := staticEntryCRD()
	if err != nil {
		return err
	}
	return c.startup(ctx, "establish CRD", func(ctx context.Context) (bool, error) {
		current, err := c.crds.Get(ctx, desired.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			current, err = c.crds.Create(ctx, desired, metav1.CreateOptions{})
			if err == nil {
				c.logger.Info("Created SPIRE ClusterStaticEntry CRD", "name", desired.Name)
			}
		}
		if err != nil {
			return false, err
		}
		if current.DeletionTimestamp != nil {
			return false, fmt.Errorf("SPIRE CRD %q is being deleted", current.Name)
		}
		// Existing schemas may have another owner. Never upgrade or adopt them
		// implicitly, including when Cilium CRD creation is skipped.
		apiextensionsv1.SetObjectDefaults_CustomResourceDefinition(current)
		if !apiequality.Semantic.DeepEqual(current.Spec, desired.Spec) {
			return false, fmt.Errorf("SPIRE CRD %q differs from the embedded schema; review schema compatibility before starting HA", current.Name)
		}
		established := false
		for _, condition := range current.Status.Conditions {
			if condition.Type == apiextensionsv1.NamesAccepted && condition.Status == apiextensionsv1.ConditionFalse {
				return false, fmt.Errorf("SPIRE CRD names not accepted: %s: %s", condition.Reason, condition.Message)
			}
			if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
				established = true
			}
		}
		return established, nil
	})
}

func (c *CRDClient) ensureBootstraps(ctx context.Context) error {
	return c.startup(ctx, "ensure bootstrap entries", func(ctx context.Context) (bool, error) {
		var missing []*unstructured.Unstructured
		for _, desired := range c.bootstraps {
			current, err := c.client.Get(ctx, desired.GetName(), metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				missing = append(missing, desired)
				continue
			}
			if err != nil {
				return false, fmt.Errorf("get bootstrap %q: %w", desired.GetName(), err)
			}
			if current.GetDeletionTimestamp() != nil {
				return false, fmt.Errorf("bootstrap %q is being deleted", current.GetName())
			}
			if current.GetLabels()[crdManagedByLabel] != bootstrapManagedBy ||
				current.GetAnnotations()["meta.helm.sh/release-name"] != "" ||
				current.GetAnnotations()["meta.helm.sh/release-namespace"] != "" ||
				len(current.GetOwnerReferences()) != 0 {
				return false, fmt.Errorf("bootstrap %q has conflicting ownership; complete the explicit ownership handoff before starting HA", current.GetName())
			}
			if !apiequality.Semantic.DeepEqual(current.Object["spec"], desired.Object["spec"]) {
				return false, fmt.Errorf("bootstrap %q has an incompatible spec; refusing to overwrite registration intent", current.GetName())
			}
		}
		for _, desired := range missing {
			if _, err := c.client.Create(ctx, desired, metav1.CreateOptions{}); err != nil {
				if apierrors.IsNotFound(err) {
					// Establishment can precede serving the new resource endpoint.
					c.logger.Warn("SPIRE bootstrap API is not yet available", "error", err)
					return false, nil
				}
				return false, fmt.Errorf("create bootstrap %q: %w", desired.GetName(), err)
			}
			c.logger.Info("Created SPIRE bootstrap entry", "name", desired.GetName())
		}
		// Read back creations through the same conflict checks before releasing
		// the workload reconciler, including after an AlreadyExists race.
		return len(missing) == 0, nil
	})
}
