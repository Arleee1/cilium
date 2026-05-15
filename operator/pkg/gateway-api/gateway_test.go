// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package gateway_api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestRemoveGVKByKind(t *testing.T) {
	tlsRouteGVK := schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1alpha2", Kind: "TLSRoute"}
	serviceImportGVK := schema.GroupVersionKind{Group: "multicluster.x-k8s.io", Version: "v1alpha1", Kind: "ServiceImport"}
	httpRouteGVK := schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}

	tests := []struct {
		name     string
		gvks     []schema.GroupVersionKind
		kind     string
		expected []schema.GroupVersionKind
	}{
		{
			name:     "remove TLSRoute from list",
			gvks:     []schema.GroupVersionKind{tlsRouteGVK, serviceImportGVK},
			kind:     "TLSRoute",
			expected: []schema.GroupVersionKind{serviceImportGVK},
		},
		{
			name:     "kind not present",
			gvks:     []schema.GroupVersionKind{serviceImportGVK, httpRouteGVK},
			kind:     "TLSRoute",
			expected: []schema.GroupVersionKind{serviceImportGVK, httpRouteGVK},
		},
		{
			name:     "empty list",
			gvks:     []schema.GroupVersionKind{},
			kind:     "TLSRoute",
			expected: []schema.GroupVersionKind{},
		},
		{
			name:     "remove only matching kind",
			gvks:     []schema.GroupVersionKind{tlsRouteGVK},
			kind:     "TLSRoute",
			expected: []schema.GroupVersionKind{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := removeGVKByKind(tt.gvks, tt.kind)
			assert.Equal(t, tt.expected, result)
		})
	}
}
