// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package spire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/hivetest"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	ztunnel "github.com/cilium/cilium/operator/pkg/ztunnel/config"
	"github.com/cilium/cilium/pkg/hive"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client"
	k8sTest "github.com/cilium/cilium/pkg/k8s/client/testutils"
)

func testRegistrationConfig() registrationConfig {
	cfg := defaultRegistrationConfig
	cfg.ServiceAccounts = maps.Clone(cfg.ServiceAccounts)
	return cfg
}

func TestRegistrationResources(t *testing.T) {
	for _, class := range []string{"", "cilium-spire", "custom-class"} {
		t.Run("class="+class, func(t *testing.T) {
			cfg := testRegistrationConfig()
			cfg.ClassName = class
			cfg.NamePrefix = "cilium-spire-network-system-custom"
			cfg.ServiceAccounts = map[string]string{
				"spire-agent":     "identity-system/agent-custom",
				"cilium-agent":    "network-system/cilium-custom",
				"cilium-operator": "network-system/operator-custom",
				"ztunnel":         "network-system/ztunnel-custom",
			}
			cfg.Labels = map[string]string{"team": "networking"}
			cfg.Annotations = map[string]string{"owner": "platform"}
			resources, err := registrationResources(cfg, "workload.test")
			require.NoError(t, err)
			require.Len(t, resources, 5)
			for _, resource := range resources {
				object := resource.object
				require.Empty(t, object.GetNamespace())
				require.Equal(t, "spire.spiffe.io/v1alpha1", object.GetAPIVersion())
				require.True(t, strings.HasPrefix(object.GetName(), cfg.NamePrefix+"-"))
				require.Equal(t, map[string]string{"team": "networking", registrationManagedByLabel: registrationManager}, object.GetLabels())
				require.Equal(t, cfg.Annotations, object.GetAnnotations())
				value, found, err := unstructured.NestedString(object.Object, "spec", "className")
				require.NoError(t, err)
				require.Equal(t, class, value)
				require.Equal(t, class != "", found)
			}
			agentID := "spiffe://workload.test/ns/identity-system/sa/agent-custom"
			for i, component := range []string{"spire-agent", "cilium-agent", "cilium-operator", "ztunnel"} {
				resource := resources[i]
				require.Equal(t, "ClusterStaticEntry", resource.object.GetKind())
				require.Equal(t, "clusterstaticentries", resource.gvr.Resource)
				spec := resource.object.Object["spec"].(map[string]any)
				if i == 0 {
					require.Equal(t, agentID, spec["spiffeID"])
					require.Equal(t, "spiffe://workload.test/spire/server", spec["parentID"])
					require.Equal(t, []any{"k8s_psat:agent_ns:identity-system", "k8s_psat:agent_sa:agent-custom"}, spec["selectors"])
				} else {
					require.Equal(t, "spiffe://workload.test/"+component, spec["spiffeID"])
					require.Equal(t, agentID, spec["parentID"])
					namespace, account, _ := strings.Cut(cfg.ServiceAccounts[component], "/")
					require.Equal(t, []any{"k8s:ns:" + namespace, "k8s:sa:" + account}, spec["selectors"])
				}
			}
			workloads := resources[4]
			require.Equal(t, "ClusterSPIFFEID", workloads.object.GetKind())
			require.Equal(t, "clusterspiffeids", workloads.gvr.Resource)
			spec := workloads.object.Object["spec"].(map[string]any)
			require.Equal(t, map[string]any{"matchLabels": map[string]any{"io.cilium/mtls-enabled": "true"}}, spec["namespaceSelector"])
			require.Equal(t, workloadIDTemplate, spec["spiffeIDTemplate"])
			require.NotContains(t, spec, "podSelector")
			require.Equal(t, map[string]string{"team": "networking"}, cfg.Labels)
		})
	}
}

func TestRegistrationConfigurationErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*registrationConfig)
		err    string
	}{
		{"empty prefix", func(c *registrationConfig) { c.NamePrefix = "" }, "must not be empty"},
		{"invalid prefix", func(c *registrationConfig) { c.NamePrefix = "BAD" }, "resource name prefix"},
		{"long prefix", func(c *registrationConfig) { c.NamePrefix = strings.Repeat("a", 250) }, "resource name prefix"},
		{"missing account", func(c *registrationConfig) { delete(c.ServiceAccounts, "spire-agent") }, "service account for spire-agent"},
		{"invalid account", func(c *registrationConfig) { c.ServiceAccounts["ztunnel"] = "network-system/bad/name" }, "service account for ztunnel"},
		{"invalid namespace", func(c *registrationConfig) { c.ServiceAccounts["ztunnel"] = "bad_namespace/ztunnel" }, "service account for ztunnel"},
		{"unknown component", func(c *registrationConfig) { c.ServiceAccounts["unknown"] = "default/default" }, "must contain only"},
		{"foreign manager", func(c *registrationConfig) { c.Labels = map[string]string{registrationManagedByLabel: "Helm"} }, "must be \"cilium-operator\""},
		{"Helm release", func(c *registrationConfig) { c.Annotations = map[string]string{"meta.helm.sh/release-name": "cilium"} }, "Helm ownership"},
		{"Helm namespace", func(c *registrationConfig) {
			c.Annotations = map[string]string{"meta.helm.sh/release-namespace": "kube-system"}
		}, "Helm ownership"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testRegistrationConfig()
			tt.change(&cfg)
			_, err := registrationResources(cfg, "spiffe.cilium")
			require.ErrorContains(t, err, tt.err)
		})
	}
	_, err := registrationResources(testRegistrationConfig(), "bad domain")
	require.ErrorContains(t, err, "trust domain")
}

func newTestRegistrationInitializer(t *testing.T) (*registrationInitializer, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	resources, err := registrationResources(testRegistrationConfig(), "spiffe.cilium")
	require.NoError(t, err)
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	return &registrationInitializer{client: client, resources: resources}, client
}

func TestRegistrationInitializationIdempotency(t *testing.T) {
	r, client := newTestRegistrationInitializer(t)
	require.NoError(t, r.initialize(t.Context()))
	for _, resource := range r.resources {
		object, err := client.Resource(resource.gvr).Get(t.Context(), resource.object.GetName(), metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, resource.object.Object, object.Object)
	}
	client.ClearActions()
	require.NoError(t, r.initialize(t.Context()))
	require.Len(t, client.Actions(), 5)
	for _, action := range client.Actions() {
		require.Equal(t, "get", action.GetVerb(), "unchanged resources must not be written")
	}

	target := r.resources[4]
	object, err := client.Tracker().Get(target.gvr, "", target.object.GetName())
	require.NoError(t, err)
	current := object.(*unstructured.Unstructured)
	current.SetResourceVersion("7")
	current.SetFinalizers([]string{"example.org/keep"})
	current.SetLabels(map[string]string{registrationManagedByLabel: registrationManager, "old": "label"})
	current.SetAnnotations(map[string]string{"old": "annotation"})
	current.Object["status"] = map[string]any{"stats": map[string]any{"entriesToSet": int64(3)}}
	require.NoError(t, unstructured.SetNestedField(current.Object, "drifted", "spec", "className"))
	require.NoError(t, client.Tracker().Update(target.gvr, current, ""))
	client.ClearActions()
	require.NoError(t, r.initialize(t.Context()))
	updated, err := client.Resource(target.gvr).Get(t.Context(), target.object.GetName(), metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, target.object.Object["spec"], updated.Object["spec"])
	require.Equal(t, current.Object["status"], updated.Object["status"])
	require.Equal(t, current.GetFinalizers(), updated.GetFinalizers())
	require.Equal(t, "7", updated.GetResourceVersion())
	require.Equal(t, target.object.GetLabels(), updated.GetLabels())
	require.Empty(t, updated.GetAnnotations())

	require.NoError(t, client.Resource(target.gvr).Delete(t.Context(), target.object.GetName(), metav1.DeleteOptions{}))
	require.NoError(t, r.initialize(t.Context()))
	recreated, err := client.Resource(target.gvr).Get(t.Context(), target.object.GetName(), metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, target.object.Object, recreated.Object)

	cfg := testRegistrationConfig()
	cfg.ClassName = ""
	cfg.ServiceAccounts["spire-agent"] = "identity-system/agent-custom"
	cfg.ServiceAccounts["ztunnel"] = "network-system/ztunnel-custom"
	r.resources, err = registrationResources(cfg, "workload.test")
	require.NoError(t, err)
	require.NoError(t, r.initialize(t.Context()))
	for _, resource := range r.resources {
		object, err := client.Resource(resource.gvr).Get(t.Context(), resource.object.GetName(), metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, resource.object.Object["spec"], object.Object["spec"])
	}
}

func TestRegistrationRefusesOtherWriters(t *testing.T) {
	for _, tt := range []struct {
		name        string
		labels      map[string]string
		annotations map[string]string
	}{
		{name: "unmanaged"},
		{name: "Helm", labels: map[string]string{registrationManagedByLabel: "Helm"}},
		{name: "Helm release", labels: map[string]string{registrationManagedByLabel: registrationManager}, annotations: map[string]string{"meta.helm.sh/release-name": "cilium"}},
		{name: "Helm namespace", labels: map[string]string{registrationManagedByLabel: registrationManager}, annotations: map[string]string{"meta.helm.sh/release-namespace": "kube-system"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, client := newTestRegistrationInitializer(t)
			target := r.resources[0]
			object := target.object.DeepCopy()
			object.SetLabels(tt.labels)
			object.SetAnnotations(tt.annotations)
			require.NoError(t, client.Tracker().Create(target.gvr, object, ""))
			require.ErrorContains(t, r.initialize(t.Context()), "refusing to overwrite")
			current, err := client.Resource(target.gvr).Get(t.Context(), object.GetName(), metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, object.Object, current.Object)
		})
	}
}

func TestRegistrationTransientErrors(t *testing.T) {
	resource := schema.GroupResource{Group: "spire.spiffe.io", Resource: "clusterstaticentries"}
	for _, tt := range []struct {
		name      string
		err       error
		transient bool
	}{
		{"no error", nil, false},
		{"forbidden", apierrors.NewForbidden(resource, "", errors.New("denied")), false},
		{"unauthorized", apierrors.NewUnauthorized("denied"), false},
		{"invalid request", apierrors.NewBadRequest("invalid spec"), false},
		{"ownership", errors.New("refusing to overwrite"), false},
		{"missing CRD", apierrors.NewNotFound(resource, ""), true},
		{"concurrent create", apierrors.NewAlreadyExists(resource, ""), true},
		{"conflict", apierrors.NewConflict(resource, "", errors.New("status changed")), true},
		{"server timeout", apierrors.NewServerTimeout(resource, "create", 1), true},
		{"timeout", apierrors.NewTimeoutError("timed out", 1), true},
		{"rate limit", apierrors.NewTooManyRequests("throttled", 1), true},
		{"unavailable", apierrors.NewServiceUnavailable("unavailable"), true},
		{"internal error", apierrors.NewInternalError(errors.New("unavailable")), true},
		{"connection refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), true},
		{"connection reset", syscall.ECONNRESET, true},
		{"host unreachable", syscall.EHOSTUNREACH, true},
		{"network unreachable", syscall.ENETUNREACH, true},
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.transient, isTransientRegistrationError(tt.err))
		})
	}
}

func TestRegistrationRetriesErrors(t *testing.T) {
	for _, verb := range []string{"get", "create", "update"} {
		t.Run(verb, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r, client := newTestRegistrationInitializer(t)
				r.resources = r.resources[:1]
				if verb == "update" {
					require.NoError(t, r.initialize(t.Context()))
					r.resources[0].object.SetAnnotations(map[string]string{"new": "annotation"})
				}
				attempts := 0
				client.PrependReactor(verb, "clusterstaticentries", func(k8stesting.Action) (bool, runtime.Object, error) {
					attempts++
					if attempts <= 2 {
						return true, nil, apierrors.NewServiceUnavailable("API server restarting")
					}
					return false, nil, nil
				})
				start := time.Now()
				require.NoError(t, r.initialize(t.Context()))
				require.Equal(t, 3, attempts)
				require.Equal(t, 3*time.Second, time.Since(start))
				target := r.resources[0]
				object, err := client.Tracker().Get(target.gvr, "", target.object.GetName())
				require.NoError(t, err)
				require.Equal(t, target.object.Object, object.(*unstructured.Unstructured).Object)
			})
		})
	}

	t.Run("status update conflict", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r, client := newTestRegistrationInitializer(t)
			require.NoError(t, r.initialize(t.Context()))
			target := r.resources[0]
			r.resources[0].object.SetAnnotations(map[string]string{"new": "annotation"})
			attempts := 0
			client.PrependReactor("update", "clusterstaticentries", func(action k8stesting.Action) (bool, runtime.Object, error) {
				attempts++
				if attempts != 1 {
					require.Equal(t, "8", action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured).GetResourceVersion())
					return false, nil, nil
				}
				object, err := client.Tracker().Get(target.gvr, "", target.object.GetName())
				require.NoError(t, err)
				current := object.(*unstructured.Unstructured)
				current.SetResourceVersion("8")
				current.Object["status"] = map[string]any{"rendered": true}
				require.NoError(t, client.Tracker().Update(target.gvr, current, ""))
				return true, nil, apierrors.NewConflict(target.gvr.GroupResource(), current.GetName(), errors.New("status changed"))
			})
			require.NoError(t, r.initialize(t.Context()))
			require.Equal(t, 2, attempts)
			current, err := client.Resource(target.gvr).Get(t.Context(), target.object.GetName(), metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, map[string]any{"rendered": true}, current.Object["status"])
		})
	})

	t.Run("CRDs installed during startup", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r, client := newTestRegistrationInitializer(t)
			attempts := 0
			client.PrependReactor("create", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
				attempts++
				if attempts <= 2 {
					return true, nil, apierrors.NewNotFound(action.GetResource().GroupResource(), "")
				}
				return false, nil, nil
			})
			require.NoError(t, r.initialize(t.Context()))
			require.Equal(t, 7, attempts)
		})
	})

	t.Run("concurrent create", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r, client := newTestRegistrationInitializer(t)
			target := r.resources[4]
			attempts := 0
			client.PrependReactor("create", "clusterspiffeids", func(k8stesting.Action) (bool, runtime.Object, error) {
				attempts++
				require.NoError(t, client.Tracker().Create(target.gvr, target.object.DeepCopy(), ""))
				return true, nil, apierrors.NewAlreadyExists(target.gvr.GroupResource(), target.object.GetName())
			})
			require.NoError(t, r.initialize(t.Context()))
			require.Equal(t, 1, attempts)
		})
	})

	t.Run("resource deletion in progress", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r, client := newTestRegistrationInitializer(t)
			target := r.resources[0]
			object := target.object.DeepCopy()
			now := metav1.Now()
			object.SetDeletionTimestamp(&now)
			require.NoError(t, client.Tracker().Create(target.gvr, object, ""))
			attempts := 0
			client.PrependReactor("get", "clusterstaticentries", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.(k8stesting.GetAction).GetName() == object.GetName() {
					attempts++
					if attempts == 3 {
						require.NoError(t, client.Tracker().Delete(target.gvr, "", object.GetName()))
					}
				}
				return false, nil, nil
			})
			require.NoError(t, r.initialize(t.Context()))
			require.Equal(t, 3, attempts)
			current, err := client.Tracker().Get(target.gvr, "", object.GetName())
			require.NoError(t, err)
			require.Equal(t, target.object.Object, current.(*unstructured.Unstructured).Object)
		})
	})
}

func TestRegistrationCancellation(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		r, client := newTestRegistrationInitializer(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.ErrorIs(t, r.initialize(ctx), context.Canceled)
		require.Empty(t, client.Actions())
	})

	t.Run("deadline during retry", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r, client := newTestRegistrationInitializer(t)
			attempts := 0
			client.PrependReactor("get", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
				attempts++
				return true, nil, apierrors.NewServiceUnavailable("API server restarting")
			})
			ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := r.initialize(ctx)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.True(t, apierrors.IsServiceUnavailable(err), "preserve the last API failure")
			require.Equal(t, 2, attempts)
			require.Equal(t, 1500*time.Millisecond, time.Since(start))
		})
	})

	t.Run("initialization deadline reaches API calls", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r, _ := newTestRegistrationInitializer(t)
			r.client = blockingRegistrationClient{}
			start := time.Now()
			require.ErrorIs(t, r.initialize(t.Context()), context.DeadlineExceeded)
			require.Equal(t, 2*time.Minute, time.Since(start))
		})
	})
}

type blockingRegistrationClient struct{ dynamic.Interface }

func (blockingRegistrationClient) Resource(schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return blockingRegistrationResource{}
}

type blockingRegistrationResource struct {
	dynamic.NamespaceableResourceInterface
}

func (blockingRegistrationResource) Get(ctx context.Context, _ string, _ metav1.GetOptions, _ ...string) (*unstructured.Unstructured, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func newTestRegistrationHive(t *testing.T, client k8sClient.Clientset) *hive.Hive {
	t.Helper()
	h := hive.New(
		cell.Config(defaultClientConfig),
		cell.Config(defaultRegistrationConfig),
		cell.Config(ztunnel.DefaultConfig),
		cell.Provide(func() k8sClient.Clientset { return client }),
		cell.Invoke(registerResources),
	)
	h.Viper().Set("enable-ztunnel", true)
	h.Viper().Set("ztunnel-ca-type", string(ztunnel.CATypeSpire))
	h.Viper().Set("enable-ztunnel-spiffeid-crd", true)
	t.Cleanup(func() { require.NoError(t, h.Stop(hivetest.Logger(t), context.Background())) })
	return h
}

func TestRegistrationLifecycle(t *testing.T) {
	for _, tt := range []struct {
		name              string
		ztunnel, crd, k8s bool
		ca                ztunnel.CAType
		active            bool
	}{
		{"CRD mode", true, true, true, ztunnel.CATypeSpire, true},
		{"ztunnel disabled", false, true, true, ztunnel.CATypeSpire, false},
		{"CRD disabled", true, false, true, ztunnel.CATypeSpire, false},
		{"internal CA", true, true, true, ztunnel.CATypeInternal, false},
		{"Kubernetes disabled", true, true, false, ztunnel.CATypeSpire, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				log := hivetest.Logger(t)
				client, cs := k8sTest.NewFakeClientset(log)
				if !tt.k8s {
					client.Disable()
				}
				h := newTestRegistrationHive(t, cs)
				h.Viper().Set("enable-ztunnel", tt.ztunnel)
				h.Viper().Set("ztunnel-ca-type", string(tt.ca))
				h.Viper().Set("enable-ztunnel-spiffeid-crd", tt.crd)
				require.NoError(t, h.Populate(log))
				require.Empty(t, client.FakeDynamicClient.Actions(), "registration must wait for lifecycle startup")
				require.NoError(t, h.Start(log, t.Context()))
				if tt.active {
					require.Len(t, client.FakeDynamicClient.Actions(), 10, "all five resources must be read and created before startup completes")
				} else {
					require.Empty(t, client.FakeDynamicClient.Actions())
				}
				client.FakeDynamicClient.ClearActions()
				time.Sleep(time.Hour)
				synctest.Wait()
				require.Empty(t, client.FakeDynamicClient.Actions(), "successful startup must leave no recurring work")
				require.NoError(t, h.Stop(log, t.Context()))
				require.Empty(t, client.FakeDynamicClient.Actions(), "shutdown must not revoke identities")
			})
		})
	}
}

func TestRegistrationLifecycleRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		log := hivetest.Logger(t)
		client, cs := k8sTest.NewFakeClientset(log)
		resources, err := registrationResources(testRegistrationConfig(), "spiffe.cilium")
		require.NoError(t, err)
		h := newTestRegistrationHive(t, cs)
		require.NoError(t, h.Start(log, t.Context()))
		require.NoError(t, h.Stop(log, t.Context()))
		client.FakeDynamicClient.ClearActions()

		h = newTestRegistrationHive(t, cs)
		require.NoError(t, h.Start(log, t.Context()))
		require.Len(t, client.FakeDynamicClient.Actions(), 5)
		for _, action := range client.FakeDynamicClient.Actions() {
			require.Equal(t, "get", action.GetVerb(), "restart must not write unchanged resources")
		}

		deleted, drifted := resources[0], resources[4]
		require.NoError(t, client.FakeDynamicClient.Tracker().Delete(deleted.gvr, "", deleted.object.GetName()))
		object := drifted.object.DeepCopy()
		require.NoError(t, unstructured.SetNestedField(object.Object, "manual", "spec", "className"))
		object.SetFinalizers([]string{"example.org/keep"})
		object.Object["status"] = map[string]any{"stats": map[string]any{"entriesToSet": int64(3)}}
		require.NoError(t, client.FakeDynamicClient.Tracker().Update(drifted.gvr, object, ""))
		client.FakeDynamicClient.ClearActions()
		time.Sleep(time.Hour)
		synctest.Wait()
		require.Empty(t, client.FakeDynamicClient.Actions())
		_, err = client.FakeDynamicClient.Tracker().Get(deleted.gvr, "", deleted.object.GetName())
		require.True(t, apierrors.IsNotFound(err), "deletion must not be repaired while the operator is running")
		current, err := client.FakeDynamicClient.Tracker().Get(drifted.gvr, "", object.GetName())
		require.NoError(t, err)
		require.Equal(t, object.Object, current.(*unstructured.Unstructured).Object, "manual drift must persist until restart")
		require.NoError(t, h.Stop(log, t.Context()))

		h = newTestRegistrationHive(t, cs)
		require.NoError(t, h.Start(log, t.Context()))
		require.Len(t, client.FakeDynamicClient.Actions(), 7, "restart must read all resources, recreate one and update one")
		for _, resource := range resources {
			current, err := client.FakeDynamicClient.Tracker().Get(resource.gvr, "", resource.object.GetName())
			require.NoError(t, err)
			require.Equal(t, resource.object.Object["spec"], current.(*unstructured.Unstructured).Object["spec"])
		}
		current, err = client.FakeDynamicClient.Tracker().Get(drifted.gvr, "", object.GetName())
		require.NoError(t, err)
		require.Equal(t, object.GetFinalizers(), current.(*unstructured.Unstructured).GetFinalizers())
		require.Equal(t, object.Object["status"], current.(*unstructured.Unstructured).Object["status"])
		require.NoError(t, h.Stop(log, t.Context()))
	})
}

func TestRegistrationLifecycleFailure(t *testing.T) {
	for _, tt := range []struct {
		name     string
		failure  error
		attempts int
		duration time.Duration
	}{
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Group: "spire.spiffe.io", Resource: "clusterstaticentries"}, "", errors.New("denied")), 1, 0},
		{"transient retries exhausted", apierrors.NewServiceUnavailable("API server restarting"), 6, 31 * time.Second},
		{"missing CRD", apierrors.NewNotFound(schema.GroupResource{Group: "spire.spiffe.io", Resource: "clusterstaticentries"}, ""), 6, 31 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				log := hivetest.Logger(t)
				client, cs := k8sTest.NewFakeClientset(log)
				attempts := 0
				client.FakeDynamicClient.PrependReactor("create", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
					attempts++
					return true, nil, tt.failure
				})
				h := newTestRegistrationHive(t, cs)
				start := time.Now()
				err := h.Start(log, t.Context())
				require.ErrorIs(t, err, tt.failure, "initialization failures must fail startup")
				require.ErrorContains(t, err, "initialize SPIRE ClusterStaticEntry")
				require.Equal(t, tt.attempts, attempts)
				require.Equal(t, tt.duration, time.Since(start))
				if tt.attempts > 1 {
					require.ErrorIs(t, err, wait.ErrWaitTimeout)
				}
				client.FakeDynamicClient.ClearActions()
				time.Sleep(time.Hour)
				synctest.Wait()
				require.Empty(t, client.FakeDynamicClient.Actions(), "failed initialization must not continue retrying")
				require.NoError(t, h.Stop(log, t.Context()))
			})
		})
	}
}
