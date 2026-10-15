// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cilium/hive/hivetest"
	"github.com/cilium/statedb"
	"github.com/cilium/statedb/reconciler"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cilium/cilium/operator/pkg/ztunnel/spire"
	k8sTables "github.com/cilium/cilium/pkg/k8s/tables"
	"github.com/cilium/cilium/pkg/ztunnel/table"
)

func newTestK8sNamespace(name string, labels map[string]string) k8sTables.Namespace {
	return k8sTables.Namespace{
		Name:   name,
		Labels: labels,
	}
}

// mockSpireClient implements SpireClient for testing.
type mockSpireClient struct {
	upsertBatchFunc func(ctx context.Context, ids []string) error
	deleteBatchFunc func(ctx context.Context, ids []string) error
	upsertFunc      func(ctx context.Context, id string) error
	deleteFunc      func(ctx context.Context, id string) error
	initializedFunc func() <-chan struct{}
	initialized     chan struct{}
}

func newMockSpireClient() *mockSpireClient {
	ch := make(chan struct{})
	close(ch)
	return &mockSpireClient{
		initialized: ch,
	}
}

func (m *mockSpireClient) UpsertBatch(ctx context.Context, ids []string) error {
	if m.upsertBatchFunc != nil {
		return m.upsertBatchFunc(ctx, ids)
	}
	return nil
}

func (m *mockSpireClient) DeleteBatch(ctx context.Context, ids []string) error {
	if m.deleteBatchFunc != nil {
		return m.deleteBatchFunc(ctx, ids)
	}
	return nil
}

func (m *mockSpireClient) Upsert(ctx context.Context, id string) error {
	if m.upsertFunc != nil {
		return m.upsertFunc(ctx, id)
	}
	return nil
}

func (m *mockSpireClient) Delete(ctx context.Context, id string) error {
	if m.deleteFunc != nil {
		return m.deleteFunc(ctx, id)
	}
	return nil
}

func (m *mockSpireClient) Initialized() <-chan struct{} {
	if m.initializedFunc != nil {
		return m.initializedFunc()
	}
	return m.initialized
}

func setupTest(t *testing.T) (*statedb.DB, statedb.RWTable[*table.EnrolledNamespace], statedb.RWTable[ServiceAccount], *mockSpireClient, *EnrollmentReconciler) {
	db := statedb.New()

	enrolledTbl, err := table.NewEnrolledNamespacesTable(db)
	require.NoError(t, err)

	saTbl, err := statedb.NewTable(db, "serviceaccounts",
		ServiceAccountNamespacedNameIndex,
		ServiceAccountNamespaceIndex,
	)
	require.NoError(t, err)

	mock := newMockSpireClient()
	logger := hivetest.Logger(t)

	ops := &EnrollmentReconciler{
		db:                     db,
		logger:                 logger,
		spireClient:            mock,
		serviceAccountTable:    saTbl,
		enrolledNamespaceTable: enrolledTbl,
		metrics:                NewMetrics(),
	}

	return db, enrolledTbl, saTbl, mock, ops
}

func insertServiceAccounts(_ *testing.T, db *statedb.DB, saTbl statedb.RWTable[ServiceAccount], namespace string, names ...string) {
	txn := db.WriteTxn(saTbl)
	for _, name := range names {
		saTbl.Insert(txn, ServiceAccount{
			ServiceAccount: &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: namespace,
				},
			},
		})
	}
	txn.Commit()
}

// getMetric reads the current value of a counter with the given label values.
func getMetric(m *Metrics, method, outcome string) float64 {
	return m.EnrollmentOps.WithLabelValues(method, outcome).Get()
}

func TestEnrollmentReconciler_Update(t *testing.T) {
	tests := []struct {
		name          string
		namespace     string
		saNames       []string
		upsertErr     error
		wantErr       bool
		wantUpsertIDs []string
	}{
		{
			name:          "upserts service accounts in namespace",
			namespace:     "test-ns",
			saNames:       []string{"sa1", "sa2"},
			wantErr:       false,
			wantUpsertIDs: []string{"test-ns/sa1", "test-ns/sa2"},
		},
		{
			name:      "upsert batch error",
			namespace: "test-ns",
			saNames:   []string{"sa1"},
			upsertErr: fmt.Errorf("spire server unavailable"),
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, enrolledTbl, saTbl, mock, ops := setupTest(t)
			enrollNamespace(db, enrolledTbl, tt.namespace)

			if len(tt.saNames) > 0 {
				insertServiceAccounts(t, db, saTbl, tt.namespace, tt.saNames...)
			}

			var gotIDs []string
			mock.upsertBatchFunc = func(ctx context.Context, ids []string) error {
				gotIDs = ids
				return tt.upsertErr
			}

			ns := &table.EnrolledNamespace{
				Name:   tt.namespace,
				Status: reconciler.StatusPending(),
			}
			err := ops.Update(t.Context(), db.ReadTxn(), 0, ns)

			if tt.wantErr {
				require.Error(t, err)
				require.Equal(t, float64(len(tt.saNames)),
					getMetric(ops.metrics, LabelValueMethodUpsert, LabelValueOutcomeFail))
			} else {
				require.NoError(t, err)
				require.ElementsMatch(t, tt.wantUpsertIDs, gotIDs)
				require.Equal(t, float64(len(tt.saNames)),
					getMetric(ops.metrics, LabelValueMethodUpsert, LabelValueOutcomeSuccess))
			}
		})
	}
}

func TestEnrollmentReconciler_Delete(t *testing.T) {
	tests := []struct {
		name          string
		namespace     string
		saNames       []string
		deleteErr     error
		wantErr       bool
		wantDeleteIDs []string
	}{
		{
			name:      "empty namespace does not send a synthetic account ID",
			namespace: "empty-ns",
		},
		{
			name:          "deletes service accounts in namespace",
			namespace:     "test-ns",
			saNames:       []string{"sa1", "sa2"},
			wantErr:       false,
			wantDeleteIDs: []string{"test-ns/sa1", "test-ns/sa2"},
		},
		{
			name:      "delete batch error",
			namespace: "test-ns",
			saNames:   []string{"sa1"},
			deleteErr: fmt.Errorf("spire server unavailable"),
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, _, saTbl, mock, ops := setupTest(t)

			if len(tt.saNames) > 0 {
				insertServiceAccounts(t, db, saTbl, tt.namespace, tt.saNames...)
			}

			var gotIDs []string
			mock.deleteBatchFunc = func(ctx context.Context, ids []string) error {
				gotIDs = ids
				return tt.deleteErr
			}

			ns := &table.EnrolledNamespace{
				Name:   tt.namespace,
				Status: reconciler.StatusPending(),
			}
			err := ops.Delete(t.Context(), db.ReadTxn(), 0, ns)

			if tt.wantErr {
				require.Error(t, err)
				require.Equal(t, float64(len(tt.saNames)),
					getMetric(ops.metrics, LabelValueMethodDelete, LabelValueOutcomeFail))
			} else {
				require.NoError(t, err)
				require.ElementsMatch(t, tt.wantDeleteIDs, gotIDs)
				require.Equal(t, float64(len(tt.saNames)),
					getMetric(ops.metrics, LabelValueMethodDelete, LabelValueOutcomeSuccess))
			}
		})
	}
}

type pruningSpireClient struct {
	*mockSpireClient
	pruneFunc func(context.Context, string, func(string) bool) error
}

func (m *pruningSpireClient) Prune(ctx context.Context, namespace string, keep func(string) bool) error {
	return m.pruneFunc(ctx, namespace, keep)
}

func registrationPruner(registrations map[string]bool) func(context.Context, string, func(string) bool) error {
	return func(_ context.Context, namespace string, keep func(string) bool) error {
		for id := range registrations {
			if (namespace == "" || strings.HasPrefix(id, namespace+"/")) && !keep(id) {
				delete(registrations, id)
			}
		}
		return nil
	}
}

func TestEnrollmentReconciler_RetriesFailedAccountDeletion(t *testing.T) {
	for _, remaining := range [][]string{nil, {"keep"}} {
		t.Run(fmt.Sprint(remaining), func(t *testing.T) {
			db, enrolledTbl, saTbl, mock, ops := setupTest(t)
			enrollNamespace(db, enrolledTbl, "shop")
			insertServiceAccounts(t, db, saTbl, "shop", remaining...)
			registrations := map[string]bool{"shop/deleted": true}
			for _, name := range remaining {
				registrations["shop/"+name] = true
			}
			ops.spireClient = &pruningSpireClient{mock, registrationPruner(registrations)}
			mock.deleteFunc = func(context.Context, string) error {
				return errors.New("temporary delete failure")
			}
			ops.reconcileServiceAccount(t.Context(), ServiceAccount{&corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "deleted"},
			}})
			require.Contains(t, registrations, "shop/deleted")
			require.Equal(t, float64(1), getMetric(ops.metrics, LabelValueMethodDelete, LabelValueOutcomeFail))
			ns, _, found := enrolledTbl.Get(db.ReadTxn(), table.EnrolledNamespaceByName("shop"))
			require.True(t, found)
			require.Equal(t, reconciler.StatusKindPending, ns.Status.Kind)
			require.NoError(t, ops.Update(t.Context(), db.ReadTxn(), 0, ns))
			require.NotContains(t, registrations, "shop/deleted")
			require.Len(t, registrations, len(remaining))
		})
	}
}

func TestEnrollmentReconciler_PrunesAfterFailedOrStaleUpsert(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			db, enrolledTbl, saTbl, mock, ops := setupTest(t)
			enrollNamespace(db, enrolledTbl, "shop")
			insertServiceAccounts(t, db, saTbl, "shop", "deleted-during-upsert")
			registrations := map[string]bool{"shop/previously-deleted": true}
			ops.spireClient = &pruningSpireClient{mock, registrationPruner(registrations)}
			upsertErr := errors.New("upsert failed")
			mock.upsertBatchFunc = func(_ context.Context, ids []string) error {
				for _, id := range ids {
					registrations[id] = true
				}
				deleteServiceAccount(db, saTbl, "shop", "deleted-during-upsert")
				if fail {
					return upsertErr
				}
				return nil
			}
			err := ops.Update(t.Context(), db.ReadTxn(), 0, &table.EnrolledNamespace{Name: "shop"})
			if fail {
				require.ErrorIs(t, err, upsertErr)
			} else {
				require.NoError(t, err)
			}
			require.Empty(t, registrations)
		})
	}
}

func TestEnrollmentReconciler_CleanupErrorsRemainRetryable(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	enrollNamespace(db, enrolledTbl, "shop")
	insertServiceAccounts(t, db, saTbl, "shop", "keep")
	upsertErr, pruneErr := errors.New("upsert failed"), errors.New("prune failed")
	mock.upsertBatchFunc = func(context.Context, []string) error { return upsertErr }
	ops.spireClient = &pruningSpireClient{mock, func(context.Context, string, func(string) bool) error { return pruneErr }}
	err := ops.Update(t.Context(), db.ReadTxn(), 0, &table.EnrolledNamespace{Name: "shop"})
	require.ErrorIs(t, err, upsertErr)
	require.ErrorIs(t, err, pruneErr)
	require.ErrorIs(t, ops.Prune(t.Context(), db.ReadTxn(), nil), pruneErr)
	require.ErrorIs(t, ops.Delete(t.Context(), db.ReadTxn(), 0, &table.EnrolledNamespace{Name: "deleted-namespace"}), pruneErr)
}

func TestEnrollmentReconciler_CleansMissedEventsAndEmptyNamespaces(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	enrollNamespace(db, enrolledTbl, "enrolled")
	insertServiceAccounts(t, db, saTbl, "enrolled", "keep")
	insertServiceAccounts(t, db, saTbl, "unenrolled", "stale")
	registrations := map[string]bool{
		"enrolled/keep": true, "enrolled/deleted": true,
		"unenrolled/stale": true, "empty/stale": true, "gone/stale": true,
	}
	ops.spireClient = &pruningSpireClient{mock, registrationPruner(registrations)}
	require.NoError(t, ops.Delete(t.Context(), db.ReadTxn(), 0, &table.EnrolledNamespace{Name: "empty"}))
	require.NotContains(t, registrations, "empty/stale")
	require.Contains(t, registrations, "gone/stale")
	require.NoError(t, ops.Prune(t.Context(), db.ReadTxn(), nil))
	require.Equal(t, map[string]bool{"enrolled/keep": true}, registrations)
}

func TestEnrollmentReconciler_GuardsStaleEvents(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	enrollNamespace(db, enrolledTbl, "shop")
	insertServiceAccounts(t, db, saTbl, "shop", "recreated")
	staleTxn := db.ReadTxn()
	ns := &table.EnrolledNamespace{Name: "shop"}
	upserts := []string{}
	mock.upsertFunc = func(_ context.Context, id string) error {
		upserts = append(upserts, id)
		return nil
	}
	mock.deleteFunc = func(context.Context, string) error {
		t.Error("stale event deleted the recreated ServiceAccount")
		return nil
	}
	mock.deleteBatchFunc = func(context.Context, []string) error {
		t.Error("stale event deleted the reenrolled namespace")
		return nil
	}
	ops.reconcileServiceAccount(t.Context(), ServiceAccount{&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "recreated", UID: "old-uid"},
	}})
	require.Equal(t, []string{"shop/recreated"}, upserts)
	require.NoError(t, ops.Delete(t.Context(), staleTxn, 0, ns))

	txn := db.WriteTxn(enrolledTbl)
	enrolledTbl.Delete(txn, ns)
	txn.Commit()
	mock.upsertBatchFunc = func(context.Context, []string) error {
		t.Error("stale update recreated an unenrolled namespace registration")
		return nil
	}
	require.NoError(t, ops.Update(t.Context(), staleTxn, 0, ns))
}

func TestEnrollmentReconciler_CleanupWaitsForInventoriesAndBackend(t *testing.T) {
	for _, operation := range []string{"update", "delete", "prune"} {
		t.Run(operation, func(t *testing.T) {
			db, enrolledTbl, saTbl, mock, ops := setupTest(t)
			txn := db.WriteTxn(saTbl, enrolledTbl)
			saReady := saTbl.RegisterInitializer(txn, "serviceaccounts")
			nsReady := enrolledTbl.RegisterInitializer(txn, "namespaces")
			txn.Commit()
			mock.initialized = make(chan struct{})
			if operation == "update" {
				enrollNamespace(db, enrolledTbl, "shop")
			}
			called := make(chan struct{}, 1)
			ops.spireClient = &pruningSpireClient{mock, func(context.Context, string, func(string) bool) error {
				called <- struct{}{}
				return nil
			}}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				ns := &table.EnrolledNamespace{Name: "shop"}
				switch operation {
				case "update":
					done <- ops.Update(ctx, db.ReadTxn(), 0, ns)
				case "delete":
					done <- ops.Delete(ctx, db.ReadTxn(), 0, ns)
				case "prune":
					done <- ops.Prune(ctx, db.ReadTxn(), nil)
				}
			}()
			assertBlocked := func() {
				t.Helper()
				select {
				case <-called:
					t.Fatal("cleanup ran before all inventories and the backend initialized")
				case <-time.After(20 * time.Millisecond):
				}
			}
			assertBlocked()
			txn = db.WriteTxn(saTbl)
			saReady(txn)
			txn.Commit()
			assertBlocked()
			txn = db.WriteTxn(enrolledTbl)
			nsReady(txn)
			txn.Commit()
			assertBlocked()
			close(mock.initialized)
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal("cleanup did not run after initialization")
			}
			require.Len(t, called, 1)
		})
	}
}

func TestEnrollmentReconciler_Update_OnlyIncludesMatchingNamespace(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	enrollNamespace(db, enrolledTbl, "enrolled-ns")

	// Insert SAs in two different namespaces.
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sa1", "sa2")
	insertServiceAccounts(t, db, saTbl, "other-ns", "sa3")

	var gotIDs []string
	mock.upsertBatchFunc = func(ctx context.Context, ids []string) error {
		gotIDs = ids
		return nil
	}

	// Update should only include SAs from "enrolled-ns".
	ns := &table.EnrolledNamespace{
		Name:   "enrolled-ns",
		Status: reconciler.StatusPending(),
	}
	err := ops.Update(t.Context(), db.ReadTxn(), 0, ns)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"enrolled-ns/sa1", "enrolled-ns/sa2"}, gotIDs)
}

// initializeTables marks both SA and enrolled namespace tables as initialized
// so that Start() can proceed past its initialization checks.
func initializeTables(db *statedb.DB, saTbl statedb.RWTable[ServiceAccount], enrolledTbl statedb.RWTable[*table.EnrolledNamespace]) {
	txn := db.WriteTxn(saTbl, enrolledTbl)
	completeSA := saTbl.RegisterInitializer(txn, "test")
	completeNS := enrolledTbl.RegisterInitializer(txn, "test")
	completeSA(txn)
	completeNS(txn)
	txn.Commit()
}

func enrollNamespace(db *statedb.DB, enrolledTbl statedb.RWTable[*table.EnrolledNamespace], name string) {
	txn := db.WriteTxn(enrolledTbl)
	enrolledTbl.Insert(txn, &table.EnrolledNamespace{
		Name:   name,
		Status: reconciler.StatusPending(),
	})
	txn.Commit()
}

func deleteServiceAccount(db *statedb.DB, saTbl statedb.RWTable[ServiceAccount], namespace, name string) {
	txn := db.WriteTxn(saTbl)
	saTbl.Delete(txn, ServiceAccount{
		ServiceAccount: &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
		},
	})
	txn.Commit()
}

func TestEnrollmentReconciler_Start_UpsertOnSACreate(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)

	// Enroll a namespace before starting.
	enrollNamespace(db, enrolledTbl, "enrolled-ns")

	upserted := make(chan string, 10)
	mock.upsertFunc = func(_ context.Context, id string) error {
		upserted <- id
		return nil
	}

	require.NoError(t, ops.Start(t.Context()))
	defer ops.Stop(nil)

	// Add a service account in the enrolled namespace.
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sa1")

	select {
	case id := <-upserted:
		require.Equal(t, "enrolled-ns/sa1", id)
		require.Eventually(t, func() bool {
			return getMetric(ops.metrics, LabelValueMethodUpsert, LabelValueOutcomeSuccess) == 1
		}, 5*time.Second, 10*time.Millisecond)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upsert")
	}
}

func TestEnrollmentReconciler_Start_SkipNonEnrolledNamespace(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)

	// Enroll "enrolled-ns" but NOT "other-ns".
	enrollNamespace(db, enrolledTbl, "enrolled-ns")

	upserted := make(chan string, 10)
	mock.upsertFunc = func(_ context.Context, id string) error {
		upserted <- id
		return nil
	}

	require.NoError(t, ops.Start(t.Context()))
	defer ops.Stop(nil)

	// Add a service account in the non-enrolled namespace first.
	insertServiceAccounts(t, db, saTbl, "other-ns", "sa1")

	// Then add one in the enrolled namespace. When we see this upsert,
	// we know the watcher has processed past the non-enrolled SA.
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sentinel")

	select {
	case id := <-upserted:
		require.Equal(t, "enrolled-ns/sentinel", id, "only the enrolled namespace SA should be upserted")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for sentinel upsert")
	}
}

func TestEnrollmentReconciler_Start_DeleteOnSARemove(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)

	enrollNamespace(db, enrolledTbl, "enrolled-ns")

	// Pre-populate a SA so we can delete it.
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sa1")

	upserted := make(chan string, 10)
	deleted := make(chan string, 10)
	mock.upsertFunc = func(_ context.Context, id string) error {
		upserted <- id
		return nil
	}
	mock.deleteFunc = func(_ context.Context, id string) error {
		deleted <- id
		return nil
	}

	require.NoError(t, ops.Start(t.Context()))
	defer ops.Stop(nil)

	// Wait for the initial SA creation to be processed.
	select {
	case <-upserted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial upsert")
	}

	// Now delete the SA.
	deleteServiceAccount(db, saTbl, "enrolled-ns", "sa1")

	select {
	case id := <-deleted:
		require.Equal(t, "enrolled-ns/sa1", id)
		require.Eventually(t, func() bool {
			return getMetric(ops.metrics, LabelValueMethodDelete, LabelValueOutcomeSuccess) == 1
		}, 5*time.Second, 10*time.Millisecond)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for delete")
	}
}

func TestEnrollmentReconciler_Start_ErrorMetricsAndRequeue(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)

	enrollNamespace(db, enrolledTbl, "enrolled-ns")

	upsertCalled := make(chan struct{}, 10)
	mock.upsertFunc = func(_ context.Context, id string) error {
		upsertCalled <- struct{}{}
		return fmt.Errorf("spire unavailable")
	}

	require.NoError(t, ops.Start(t.Context()))
	defer ops.Stop(nil)

	// Add a SA — upsert should be called even though it errors.
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sa1")

	select {
	case <-upsertCalled:
		require.Eventually(t, func() bool {
			return getMetric(ops.metrics, LabelValueMethodUpsert, LabelValueOutcomeFail) == 1
		}, 5*time.Second, 10*time.Millisecond)

		// Verify the namespace was requeued to pending for re-reconciliation.
		require.Eventually(t, func() bool {
			ns, _, found := enrolledTbl.Get(db.ReadTxn(), table.EnrolledNamespaceByName("enrolled-ns"))
			return found && ns.Status.Kind == reconciler.StatusKindPending
		}, 5*time.Second, 10*time.Millisecond)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upsert call")
	}
}

func TestEnrollmentReconciler_Start_WaitsForSpireInit(t *testing.T) {
	db, enrolledTbl, saTbl, _, ops := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)

	enrollNamespace(db, enrolledTbl, "enrolled-ns")

	// Create a mock with uninitialized SPIRE client.
	spireInitCh := make(chan struct{})
	mock := &mockSpireClient{
		initialized: spireInitCh,
	}
	upserted := make(chan string, 10)
	mock.upsertFunc = func(_ context.Context, id string) error {
		upserted <- id
		return nil
	}
	ops.spireClient = mock

	require.NoError(t, ops.Start(t.Context()))
	defer ops.Stop(nil)

	// Add a SA before SPIRE is initialized.
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sa1")

	// Verify that no upsert happens while SPIRE is not initialized.
	select {
	case id := <-upserted:
		t.Fatalf("unexpected upsert before SPIRE init: %s", id)
	case <-time.After(200 * time.Millisecond):
		// Expected.
	}

	// Now initialize SPIRE.
	close(spireInitCh)

	// The SA change should be processed now.
	select {
	case id := <-upserted:
		require.Equal(t, "enrolled-ns/sa1", id)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upsert after SPIRE init")
	}
}

func TestEnrollmentReconciler_Start_DeletionDuringSpireInit(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)
	enrollNamespace(db, enrolledTbl, "enrolled-ns")
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sa1")
	_, prunes := ops.spireClient.(spire.ServiceAccountPruner)
	require.False(t, prunes)

	mock.initialized = make(chan struct{})
	waiting := make(chan struct{})
	signalWaiting := sync.OnceFunc(func() { close(waiting) })
	release := sync.OnceFunc(func() { close(mock.initialized) })
	mock.initializedFunc = func() <-chan struct{} {
		signalWaiting()
		return mock.initialized
	}
	var registered sync.Map
	registered.Store("enrolled-ns/sa1", struct{}{})
	deleted := make(chan string, 10)
	mock.deleteFunc = func(_ context.Context, id string) error {
		registered.Delete(id)
		deleted <- id
		return nil
	}
	require.NoError(t, ops.Start(t.Context()))
	t.Cleanup(func() {
		release()
		require.NoError(t, ops.Stop(nil))
	})

	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for backend readiness wait")
	}
	deleteServiceAccount(db, saTbl, "enrolled-ns", "sa1")
	select {
	case id := <-deleted:
		t.Fatalf("unexpected deletion before SPIRE initialization: %s", id)
	default:
	}
	release()

	select {
	case id := <-deleted:
		require.Equal(t, "enrolled-ns/sa1", id)
		_, remains := registered.Load(id)
		require.False(t, remains)
	case <-time.After(5 * time.Second):
		t.Fatal("deletion during SPIRE initialization was lost")
	}
}

func TestEnrollmentReconciler_Start_CancelDuringSpireInit(t *testing.T) {
	db, enrolledTbl, saTbl, mock, ops := setupTest(t)
	initializeTables(db, saTbl, enrolledTbl)
	enrollNamespace(db, enrolledTbl, "enrolled-ns")
	insertServiceAccounts(t, db, saTbl, "enrolled-ns", "sa1")
	mock.initialized = make(chan struct{})
	waiting := make(chan struct{})
	signalWaiting := sync.OnceFunc(func() { close(waiting) })
	release := sync.OnceFunc(func() { close(mock.initialized) })
	mock.initializedFunc = func() <-chan struct{} {
		signalWaiting()
		return mock.initialized
	}
	mutations := make(chan string, 10)
	mock.upsertFunc = func(_ context.Context, id string) error {
		mutations <- id
		return nil
	}
	mock.deleteFunc = mock.upsertFunc
	require.NoError(t, ops.Start(t.Context()))
	t.Cleanup(func() {
		release()
		require.NoError(t, ops.Stop(nil))
	})

	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for backend readiness wait")
	}
	deleteServiceAccount(db, saTbl, "enrolled-ns", "sa1")
	stopped := make(chan error, 1)
	go func() { stopped <- ops.Stop(nil) }()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop while SPIRE initialization was pending")
	}
	release()
	select {
	case id := <-mutations:
		t.Fatalf("unexpected mutation after cancellation: %s", id)
	default:
	}
}

func TestK8sNamespaceToEnrolledNamespace_LabelMatching(t *testing.T) {
	tests := []struct {
		name       string
		labels     map[string]string
		deleted    bool
		wantResult statedb.DeriveResult
	}{
		{
			name:       "enrolled namespace",
			labels:     map[string]string{"io.cilium/mtls-enabled": "true"},
			wantResult: statedb.DeriveInsert,
		},
		{
			name:       "not enrolled - label missing",
			labels:     map[string]string{},
			wantResult: statedb.DeriveDelete,
		},
		{
			name:       "enrolled but deleted",
			labels:     map[string]string{"io.cilium/mtls-enabled": "true"},
			deleted:    true,
			wantResult: statedb.DeriveDelete,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := newTestK8sNamespace(tt.name, tt.labels)
			_, result := table.K8sNamespaceToEnrolledNamespace(ns, tt.deleted)
			require.Equal(t, tt.wantResult, result)
		})
	}
}
