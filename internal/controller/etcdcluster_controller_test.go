/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"testing"
	"time"

	certv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ecv1alpha1 "go.etcd.io/etcd-operator/api/v1alpha1"
	"go.etcd.io/etcd-operator/internal/etcdutils"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// TestFetchAndValidateState describes the scenarios for the fetchAndValidateState
// helper. Each sub-test will set up a fake client with different existing
// resources and assert on the returned state, result and error.
func TestFetchAndValidateState(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)

	cases := []struct {
		name   string
		req    ctrl.Request
		ec     *ecv1alpha1.EtcdCluster
		sts    *appsv1.StatefulSet
		assert func(t *testing.T, state *reconcileState, res ctrl.Result, err error, ec *ecv1alpha1.EtcdCluster, sts *appsv1.StatefulSet)
	}{
		{
			name: "EtcdCluster Not Found",
			req:  ctrl.Request{NamespacedName: types.NamespacedName{Name: "etcd", Namespace: "default"}},
			assert: func(t *testing.T, state *reconcileState, res ctrl.Result, err error, _ *ecv1alpha1.EtcdCluster, _ *appsv1.StatefulSet) {
				assert.Nil(t, state)
				assert.NoError(t, err)
				assert.Equal(t, ctrl.Result{}, res)
			},
		},
		{
			name: "StatefulSet Not Found",
			ec: &ecv1alpha1.EtcdCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "etcd",
					Namespace: "default",
					UID:       "1",
				},
				Spec: ecv1alpha1.EtcdClusterSpec{Size: 1, Version: "3.5.17"},
			},
			req: ctrl.Request{NamespacedName: types.NamespacedName{Name: "etcd", Namespace: "default"}},
			assert: func(t *testing.T, state *reconcileState, res ctrl.Result, err error, ec *ecv1alpha1.EtcdCluster, _ *appsv1.StatefulSet) {
				require.NotNil(t, state)
				assert.Equal(t, ec.Name, state.cluster.Name)
				assert.Nil(t, state.sts)
				assert.NoError(t, err)
				assert.Equal(t, ctrl.Result{}, res)
			},
		},
		{
			name: "Resources Exist and Owned",
			ec: &ecv1alpha1.EtcdCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "etcd",
					Namespace: "default",
					UID:       "2",
				},
				Spec: ecv1alpha1.EtcdClusterSpec{Size: 1, Version: "3.5.17"},
			},
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "etcd",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: ecv1alpha1.GroupVersion.String(),
							Kind:       "EtcdCluster",
							Name:       "etcd",
							UID:        "2",
							Controller: pointerToBool(true),
						},
					},
				},
			},
			req: ctrl.Request{NamespacedName: types.NamespacedName{Name: "etcd", Namespace: "default"}},
			assert: func(t *testing.T, state *reconcileState, res ctrl.Result, err error, ec *ecv1alpha1.EtcdCluster, sts *appsv1.StatefulSet) {
				require.NotNil(t, state)
				assert.Equal(t, ec.Name, state.cluster.Name)
				require.NotNil(t, state.sts)
				assert.Equal(t, sts.Name, state.sts.Name)
				assert.NoError(t, err)
				assert.Equal(t, ctrl.Result{}, res)
			},
		},
		{
			name: "StatefulSet Not Owned",
			ec: &ecv1alpha1.EtcdCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "etcd",
					Namespace: "default",
					UID:       "3",
				},
				Spec: ecv1alpha1.EtcdClusterSpec{Size: 1, Version: "3.5.17"},
			},
			sts: &appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "etcd",
					Namespace: "default",
				},
			},
			req: ctrl.Request{NamespacedName: types.NamespacedName{Name: "etcd", Namespace: "default"}},
			assert: func(t *testing.T, state *reconcileState, res ctrl.Result, err error, _ *ecv1alpha1.EtcdCluster, _ *appsv1.StatefulSet) {
				assert.Nil(t, state)
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "not controlled")
				assert.Equal(t, ctrl.Result{}, res)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()

			objs := []client.Object{}
			if tc.ec != nil {
				objs = append(objs, tc.ec)
			}
			if tc.sts != nil {
				objs = append(objs, tc.sts)
			}

			builder := fake.NewClientBuilder().WithScheme(scheme)
			if len(objs) > 0 {
				builder.WithObjects(objs...)
			}
			fakeClient := builder.Build()

			r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}

			state, res, err := r.fetchAndValidateState(ctx, tc.req)
			tc.assert(t, state, res, err, tc.ec, tc.sts)
		})
	}
}

// TestBootstrapStatefulSet outlines tests for ensuring StatefulSet and Service
// creation and bootstrap logic.
func TestBootstrapStatefulSet(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = policyv1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)

	ec := &ecv1alpha1.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "etcd",
			Namespace: "default",
			UID:       "1",
		},
		Spec: ecv1alpha1.EtcdClusterSpec{
			Size:    1,
			Version: "3.5.17",
		},
	}

	t.Run("Initial Creation", func(t *testing.T) {
		ctx := t.Context()

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ec).Build()
		r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}
		state := &reconcileState{cluster: ec}

		res, err := r.bootstrapStatefulSet(ctx, state)
		assert.NoError(t, err)
		assert.Equal(t, ctrl.Result{RequeueAfter: requeueDuration}, res)
		require.NotNil(t, state.sts)
		assert.NotNil(t, state.sts.Spec.Replicas)
		assert.Equal(t, int32(0), *state.sts.Spec.Replicas)

		sts := &appsv1.StatefulSet{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, sts)
		assert.NoError(t, err)
		assert.NotNil(t, sts.Spec.Replicas)
		assert.Equal(t, int32(0), *sts.Spec.Replicas)

		svc := &corev1.Service{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, svc)
		assert.NoError(t, err)
		assert.Equal(t, "None", svc.Spec.ClusterIP)
		assert.True(t, svc.Spec.PublishNotReadyAddresses)

		clientSvc := &corev1.Service{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: clientServiceNameForEtcdCluster(ec), Namespace: ec.Namespace}, clientSvc)
		assert.NoError(t, err)
		assert.Equal(t, "None", clientSvc.Spec.ClusterIP)
		assert.False(t, clientSvc.Spec.PublishNotReadyAddresses)
		require.Len(t, clientSvc.Spec.Ports, 1)
		assert.Equal(t, int32(2379), clientSvc.Spec.Ports[0].Port)

		pdb := &policyv1.PodDisruptionBudget{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, pdb)
		assert.NoError(t, err)
		require.NotNil(t, pdb.Spec.MaxUnavailable)
		assert.Nil(t, pdb.Spec.MinAvailable)
		assert.Equal(t, int32(1), pdb.Spec.MaxUnavailable.IntVal)

		cm := &corev1.ConfigMap{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: configMapNameForEtcdCluster(ec), Namespace: ec.Namespace}, cm)
		assert.NoError(t, err)
	})

	t.Run("Bootstrap from Zero", func(t *testing.T) {
		ctx := t.Context()

		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ec.Name,
				Namespace: ec.Namespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: ecv1alpha1.GroupVersion.String(),
					Kind:       "EtcdCluster",
					Name:       ec.Name,
					UID:        ec.UID,
					Controller: pointerToBool(true),
				}},
			},
			Spec: appsv1.StatefulSetSpec{
				Replicas: pointerToInt32(0),
			},
			Status: appsv1.StatefulSetStatus{ReadyReplicas: 1},
		}

		cm := newEtcdClusterState(ec, 0)

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ec, sts, cm).Build()
		r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}
		state := &reconcileState{cluster: ec, sts: sts}

		oldRV := sts.ResourceVersion
		res, err := r.bootstrapStatefulSet(ctx, state)
		assert.NoError(t, err)
		assert.Equal(t, ctrl.Result{RequeueAfter: requeueDuration}, res)

		updatedSTS := &appsv1.StatefulSet{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, updatedSTS)
		assert.NoError(t, err)
		assert.Equal(t, int32(1), *updatedSTS.Spec.Replicas)
		assert.Equal(t, int32(1), updatedSTS.Status.ReadyReplicas)
		assert.NotEqual(t, oldRV, updatedSTS.ResourceVersion)

		require.NotNil(t, state.sts)
		assert.Equal(t, int32(1), *state.sts.Spec.Replicas)

		svc := &corev1.Service{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, svc)
		assert.NoError(t, err)
		assert.Equal(t, "None", svc.Spec.ClusterIP)
		assert.True(t, svc.Spec.PublishNotReadyAddresses)

		clientSvc := &corev1.Service{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: clientServiceNameForEtcdCluster(ec), Namespace: ec.Namespace}, clientSvc)
		assert.NoError(t, err)
		assert.False(t, clientSvc.Spec.PublishNotReadyAddresses)

		pdb := &policyv1.PodDisruptionBudget{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, pdb)
		assert.NoError(t, err)

		cmUpdated := &corev1.ConfigMap{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: configMapNameForEtcdCluster(ec), Namespace: ec.Namespace}, cmUpdated)
		assert.NoError(t, err)
		assert.Equal(t, "new", cmUpdated.Data["ETCD_INITIAL_CLUSTER_STATE"])
		assert.Contains(t, cmUpdated.Data["ETCD_INITIAL_CLUSTER"], "etcd-0=")
	})

	t.Run("Resources Already Exist", func(t *testing.T) {
		ctx := t.Context()

		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ec.Name,
				Namespace: ec.Namespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: ecv1alpha1.GroupVersion.String(),
					Kind:       "EtcdCluster",
					Name:       ec.Name,
					UID:        ec.UID,
					Controller: pointerToBool(true),
				}},
			},
			Spec:   appsv1.StatefulSetSpec{Replicas: pointerToInt32(1)},
			Status: appsv1.StatefulSetStatus{ReadyReplicas: 1},
		}

		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ec.Name,
				Namespace: ec.Namespace,
			},
			Spec: corev1.ServiceSpec{ClusterIP: "None"},
		}

		cm := newEtcdClusterState(ec, 1)

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ec, sts.DeepCopy(), svc.DeepCopy(), cm.DeepCopy()).Build()
		r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}
		state := &reconcileState{cluster: ec, sts: sts}

		storedSTS := sts.DeepCopy()
		storedCM := cm.DeepCopy()
		res, err := r.bootstrapStatefulSet(ctx, state)
		assert.NoError(t, err)
		assert.Equal(t, ctrl.Result{}, res)

		fetchedSTS := &appsv1.StatefulSet{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, fetchedSTS)
		assert.NoError(t, err)
		assert.Equal(t, storedSTS.Spec, fetchedSTS.Spec)

		fetchedSvc := &corev1.Service{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, fetchedSvc)
		assert.NoError(t, err)
		assert.Equal(t, "None", fetchedSvc.Spec.ClusterIP)
		assert.True(t, fetchedSvc.Spec.PublishNotReadyAddresses)
		assert.Equal(t, labelsForEtcdCluster(ec), fetchedSvc.Spec.Selector)

		clientSvc := &corev1.Service{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: clientServiceNameForEtcdCluster(ec), Namespace: ec.Namespace}, clientSvc)
		assert.NoError(t, err)

		pdb := &policyv1.PodDisruptionBudget{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, pdb)
		assert.NoError(t, err)
		require.NotNil(t, pdb.Spec.MaxUnavailable)
		assert.Equal(t, int32(1), pdb.Spec.MaxUnavailable.IntVal)

		fetchedCM := &corev1.ConfigMap{}
		err = fakeClient.Get(ctx, client.ObjectKey{Name: configMapNameForEtcdCluster(ec), Namespace: ec.Namespace}, fetchedCM)
		assert.NoError(t, err)
		assert.Equal(t, storedCM.Data, fetchedCM.Data)
	})

	t.Run("Existing OrderedReady StatefulSet Needs Migration", func(t *testing.T) {
		ctx := t.Context()
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ec.Name,
				Namespace: ec.Namespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: ecv1alpha1.GroupVersion.String(),
					Kind:       "EtcdCluster",
					Name:       ec.Name,
					UID:        ec.UID,
					Controller: pointerToBool(true),
				}},
			},
			Spec: appsv1.StatefulSetSpec{Replicas: pointerToInt32(1), PodManagementPolicy: appsv1.OrderedReadyPodManagement},
		}
		cm := newEtcdClusterState(ec, 1)
		recorder := record.NewFakeRecorder(1)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ec.DeepCopy(), sts, cm).Build()
		r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme, Recorder: recorder}
		state := &reconcileState{cluster: ec.DeepCopy(), sts: sts}

		res, err := r.bootstrapStatefulSet(ctx, state)

		assert.NoError(t, err)
		assert.Equal(t, ctrl.Result{}, res)
		cond := conditionByType(state.cluster.Status.Conditions, ecv1alpha1.EtcdClusterReady)
		require.NotNil(t, cond)
		assert.Equal(t, "StatefulSetPodManagementPolicyNeedsMigration", cond.Reason)
		select {
		case event := <-recorder.Events:
			assert.Contains(t, event, "StatefulSetPodManagementPolicyNeedsMigration")
		case <-time.After(time.Second):
			t.Fatal("expected warning event")
		}
	})

}

func TestValidateEtcdVersionUpgrade(t *testing.T) {
	tests := []struct {
		name    string
		current string
		desired string
		wantErr string
	}{
		{name: "patch upgrade allowed", current: "3.5.17", desired: "3.5.18"},
		{name: "adjacent minor upgrade allowed", current: "v3.5.17", desired: "v3.6.0"},
		{name: "same version allowed", current: "3.5.17", desired: "3.5.17"},
		{name: "suffixed desired patch upgrade allowed", current: "v3.5.21", desired: "v3.5.28-260421"},
		{name: "suffixed patch upgrade allowed", current: "v3.5.21-100", desired: "v3.5.28-260421"},
		{name: "build metadata patch upgrade allowed", current: "3.5.21+build.1", desired: "3.5.28+build.2"},
		{name: "downgrade blocked", current: "3.5.17", desired: "3.5.16", wantErr: "downgrade"},
		{name: "suffixed downgrade blocked", current: "v3.5.28-260421", desired: "v3.5.21-100", wantErr: "downgrade"},
		{name: "cross minor blocked", current: "3.4.0", desired: "3.6.0", wantErr: "cross-minor"},
		{name: "suffixed major change blocked", current: "v3.5.28-260421", desired: "v4.0.0-1", wantErr: "major version change"},
		{name: "major change blocked", current: "3.5.17", desired: "4.0.0", wantErr: "major version change"},
		{name: "invalid desired blocked", current: "3.5.17", desired: "bad", wantErr: "desired version"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEtcdVersionUpgrade(tt.current, tt.desired)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func conditionByType(conditions []metav1.Condition, conditionType ecv1alpha1.EtcdClusterConditionType) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == string(conditionType) {
			return &conditions[i]
		}
	}
	return nil
}

func statusTestCluster(size int) *ecv1alpha1.EtcdCluster {
	return &ecv1alpha1.EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "etcd", Namespace: "default", UID: "status", Generation: 7},
		Spec:       ecv1alpha1.EtcdClusterSpec{Size: size, Version: "3.5.17"},
	}
}

func statusTestStatefulSet(ec *ecv1alpha1.EtcdCluster, readyReplicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: ec.Name, Namespace: ec.Namespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas: pointerToInt32(int32(ec.Spec.Size)),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "etcd", Image: "registry/etcd:" + ec.Spec.Version}}}},
		},
		Status: appsv1.StatefulSetStatus{Replicas: int32(ec.Spec.Size), ReadyReplicas: readyReplicas, UpdatedReplicas: int32(ec.Spec.Size), CurrentRevision: "rev", UpdateRevision: "rev"},
	}
}

func statusMemberList(members ...*etcdserverpb.Member) *clientv3.MemberListResponse {
	return &clientv3.MemberListResponse{Members: members}
}

func TestBuildClusterStatusReady(t *testing.T) {
	ec := statusTestCluster(3)
	sts := statusTestStatefulSet(ec, 3)
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "etcd-0"}, Spec: corev1.PodSpec{NodeName: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "etcd-1"}, Spec: corev1.PodSpec{NodeName: "node-1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "etcd-2"}, Spec: corev1.PodSpec{NodeName: "node-2"}},
	}
	members := statusMemberList(
		&etcdserverpb.Member{ID: 1, Name: "etcd-0"},
		&etcdserverpb.Member{ID: 2, Name: "etcd-1"},
		&etcdserverpb.Member{ID: 3, Name: "etcd-2"},
	)
	health := []etcdutils.EpHealth{epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, true, 1), epHealth("etcd-2", 3, true, 1)}

	status := buildClusterStatus(ec, sts, pods, members, health)

	assert.Equal(t, ecv1alpha1.EtcdClusterPhaseReady, status.Phase)
	assert.Equal(t, int32(3), status.ReadyReplicas)
	assert.Equal(t, int32(3), status.MemberCount)
	assert.Equal(t, "1", status.LeaderID)
	assert.Equal(t, int64(7), status.ObservedGeneration)
	assert.Equal(t, metav1.ConditionTrue, conditionByType(status.Conditions, ecv1alpha1.EtcdClusterReady).Status)
	assert.Equal(t, metav1.ConditionTrue, conditionByType(status.Conditions, ecv1alpha1.QuorumAvailable).Status)
	assert.Equal(t, metav1.ConditionFalse, conditionByType(status.Conditions, ecv1alpha1.SingleMemberRecoveryActive).Status)
}

func TestBuildClusterStatusPendingWithoutStatefulSet(t *testing.T) {
	ec := statusTestCluster(3)

	status := buildClusterStatus(ec, nil, nil, nil, nil)

	assert.Equal(t, ecv1alpha1.EtcdClusterPhasePending, status.Phase)
	assert.Equal(t, metav1.ConditionFalse, conditionByType(status.Conditions, ecv1alpha1.EtcdClusterCreated).Status)
	assert.Equal(t, metav1.ConditionFalse, conditionByType(status.Conditions, ecv1alpha1.EtcdClusterReady).Status)
	assert.Equal(t, metav1.ConditionFalse, conditionByType(status.Conditions, ecv1alpha1.QuorumAvailable).Status)
}

func TestBuildClusterStatusDegradedWithoutQuorum(t *testing.T) {
	ec := statusTestCluster(3)
	sts := statusTestStatefulSet(ec, 1)
	members := statusMemberList(
		&etcdserverpb.Member{ID: 1, Name: "etcd-0"},
		&etcdserverpb.Member{ID: 2, Name: "etcd-1"},
		&etcdserverpb.Member{ID: 3, Name: "etcd-2"},
	)
	health := []etcdutils.EpHealth{epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, false, 1)}

	status := buildClusterStatus(ec, sts, nil, members, health)

	assert.Equal(t, ecv1alpha1.EtcdClusterPhaseDegraded, status.Phase)
	assert.Equal(t, metav1.ConditionFalse, conditionByType(status.Conditions, ecv1alpha1.EtcdClusterReady).Status)
	assert.Equal(t, metav1.ConditionFalse, conditionByType(status.Conditions, ecv1alpha1.QuorumAvailable).Status)
}

func TestBuildClusterStatusMembersSortedWithLeaderLearnerAndNode(t *testing.T) {
	ec := statusTestCluster(3)
	sts := statusTestStatefulSet(ec, 2)
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "etcd-2"}, Spec: corev1.PodSpec{NodeName: "node-2"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "etcd-0"}, Spec: corev1.PodSpec{NodeName: "node-0"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "etcd-1"}, Spec: corev1.PodSpec{NodeName: "node-1"}},
	}
	members := statusMemberList(
		&etcdserverpb.Member{ID: 3, Name: "etcd-2"},
		&etcdserverpb.Member{ID: 1, Name: "etcd-0"},
		&etcdserverpb.Member{ID: 2, Name: "etcd-1", IsLearner: true},
	)
	learnerHealth := epHealth("etcd-1", 2, true, 1)
	learnerHealth.Status.IsLearner = true
	health := []etcdutils.EpHealth{epHealth("etcd-2", 3, true, 1), epHealth("etcd-0", 1, true, 1), learnerHealth}

	status := buildClusterStatus(ec, sts, pods, members, health)

	require.Len(t, status.Members, 3)
	assert.Equal(t, "etcd-0", status.Members[0].Name)
	assert.Equal(t, "1", status.Members[0].ID)
	assert.True(t, status.Members[0].Leader)
	assert.Equal(t, "node-0", status.Members[0].NodeName)
	assert.Equal(t, "etcd-1", status.Members[1].Name)
	assert.True(t, status.Members[1].Learner)
	assert.Equal(t, "node-1", status.Members[1].NodeName)
	assert.Equal(t, "etcd-2", status.Members[2].Name)
}

func TestBuildClusterStatusMarksRunningRecoverySucceededWhenReady(t *testing.T) {
	ec := statusTestCluster(3)
	ec.Status.Recovery = &ecv1alpha1.EtcdClusterRecoveryStatus{LastResult: ecv1alpha1.EtcdRecoveryResultRunning, LastRecoveredMember: "etcd-1", RetryCount: 1}
	sts := statusTestStatefulSet(ec, 3)
	members := statusMemberList(
		&etcdserverpb.Member{ID: 1, Name: "etcd-0"},
		&etcdserverpb.Member{ID: 2, Name: "etcd-1"},
		&etcdserverpb.Member{ID: 3, Name: "etcd-2"},
	)
	health := []etcdutils.EpHealth{epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, true, 1), epHealth("etcd-2", 3, true, 1)}

	status := buildClusterStatus(ec, sts, nil, members, health)

	require.NotNil(t, status.Recovery)
	assert.Equal(t, ecv1alpha1.EtcdRecoveryResultSucceeded, status.Recovery.LastResult)
	assert.Equal(t, "previous recovery target is healthy", status.Recovery.Message)
	assert.Equal(t, "RecoverySucceeded", conditionByType(status.Conditions, ecv1alpha1.SingleMemberRecoveryActive).Reason)
}

func TestUpdateStatusPreservesVersionUpgradeBlockedCondition(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)
	ec := statusTestCluster(3)
	setCondition(&ec.Status.Conditions, metav1.Condition{Type: string(ecv1alpha1.EtcdClusterReady), Status: metav1.ConditionFalse, Reason: "EtcdVersionUpgradeBlocked", Message: "blocked"}, ec.Generation)
	sts := statusTestStatefulSet(ec, 3)
	members := statusMemberList(&etcdserverpb.Member{ID: 1, Name: "etcd-0"}, &etcdserverpb.Member{ID: 2, Name: "etcd-1"}, &etcdserverpb.Member{ID: 3, Name: "etcd-2"})
	health := []etcdutils.EpHealth{epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, true, 1), epHealth("etcd-2", 3, true, 1)}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&ecv1alpha1.EtcdCluster{}).WithObjects(ec).Build()
	r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}

	err := r.updateStatus(ctx, &reconcileState{cluster: ec, sts: sts, memberListResp: members, memberHealth: health, readyGuardConditionSet: true})

	require.NoError(t, err)
	updated := &ecv1alpha1.EtcdCluster{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, updated))
	cond := conditionByType(updated.Status.Conditions, ecv1alpha1.EtcdClusterReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "EtcdVersionUpgradeBlocked", cond.Reason)
	assert.Equal(t, ecv1alpha1.EtcdClusterPhaseDegraded, updated.Status.Phase)
}

func TestUpdateStatusDropsStaleReadyGuardCondition(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)
	ec := statusTestCluster(3)
	setCondition(&ec.Status.Conditions, metav1.Condition{Type: string(ecv1alpha1.EtcdClusterReady), Status: metav1.ConditionFalse, Reason: "EtcdVersionUpgradeBlocked", Message: "blocked"}, ec.Generation)
	sts := statusTestStatefulSet(ec, 3)
	members := statusMemberList(&etcdserverpb.Member{ID: 1, Name: "etcd-0"}, &etcdserverpb.Member{ID: 2, Name: "etcd-1"}, &etcdserverpb.Member{ID: 3, Name: "etcd-2"})
	health := []etcdutils.EpHealth{epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, true, 1), epHealth("etcd-2", 3, true, 1)}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&ecv1alpha1.EtcdCluster{}).WithObjects(ec).Build()
	r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}

	err := r.updateStatus(ctx, &reconcileState{cluster: ec, sts: sts, memberListResp: members, memberHealth: health})

	require.NoError(t, err)
	updated := &ecv1alpha1.EtcdCluster{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, updated))
	cond := conditionByType(updated.Status.Conditions, ecv1alpha1.EtcdClusterReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "ClusterReady", cond.Reason)
	assert.Equal(t, ecv1alpha1.EtcdClusterPhaseReady, updated.Status.Phase)
}

func TestUpdateStatusPreservesActiveRecoveryCondition(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)
	ec := statusTestCluster(3)
	setCondition(&ec.Status.Conditions, metav1.Condition{Type: string(ecv1alpha1.SingleMemberRecoveryActive), Status: metav1.ConditionTrue, Reason: "RecoveryJobRunning", Message: "running"}, ec.Generation)
	sts := statusTestStatefulSet(ec, 3)
	members := statusMemberList(&etcdserverpb.Member{ID: 1, Name: "etcd-0"}, &etcdserverpb.Member{ID: 2, Name: "etcd-1"}, &etcdserverpb.Member{ID: 3, Name: "etcd-2"})
	health := []etcdutils.EpHealth{epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, true, 1), epHealth("etcd-2", 3, true, 1)}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&ecv1alpha1.EtcdCluster{}).WithObjects(ec).Build()
	r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}

	err := r.updateStatus(ctx, &reconcileState{cluster: ec, sts: sts, memberListResp: members, memberHealth: health, recoveryConditionSet: true})

	require.NoError(t, err)
	updated := &ecv1alpha1.EtcdCluster{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, updated))
	cond := conditionByType(updated.Status.Conditions, ecv1alpha1.SingleMemberRecoveryActive)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "RecoveryJobRunning", cond.Reason)
	assert.Equal(t, ecv1alpha1.EtcdClusterPhaseRecovering, updated.Status.Phase)
}

func TestUpdateStatusPreservesBlockedRecoveryConditionReason(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)
	ec := statusTestCluster(3)
	setCondition(&ec.Status.Conditions, metav1.Condition{Type: string(ecv1alpha1.SingleMemberRecoveryActive), Status: metav1.ConditionFalse, Reason: "NoSpaceAlarm", Message: "blocked"}, ec.Generation)
	sts := statusTestStatefulSet(ec, 2)
	members := statusMemberList(&etcdserverpb.Member{ID: 1, Name: "etcd-0"}, &etcdserverpb.Member{ID: 2, Name: "etcd-1"}, &etcdserverpb.Member{ID: 3, Name: "etcd-2"})
	health := []etcdutils.EpHealth{epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1)}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&ecv1alpha1.EtcdCluster{}).WithObjects(ec).Build()
	r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}

	err := r.updateStatus(ctx, &reconcileState{cluster: ec, sts: sts, memberListResp: members, memberHealth: health, recoveryConditionSet: true})

	require.NoError(t, err)
	updated := &ecv1alpha1.EtcdCluster{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, updated))
	cond := conditionByType(updated.Status.Conditions, ecv1alpha1.SingleMemberRecoveryActive)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "NoSpaceAlarm", cond.Reason)
	assert.Equal(t, "blocked", cond.Message)
}

func TestUpdateStatusPersistsDataStoreConditionSetDuringReconcile(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)
	ec := statusTestCluster(3)
	sts := statusTestStatefulSet(ec, 3)
	members := statusMemberList(&etcdserverpb.Member{ID: 1, Name: "etcd-0"}, &etcdserverpb.Member{ID: 2, Name: "etcd-1"}, &etcdserverpb.Member{ID: 3, Name: "etcd-2"})
	health := []etcdutils.EpHealth{epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, true, 1), epHealth("etcd-2", 3, true, 1)}
	ec.Status = buildClusterStatus(ec, sts, nil, members, health)
	setCondition(&ec.Status.Conditions, metav1.Condition{Type: string(ecv1alpha1.DataStoreReady), Status: metav1.ConditionFalse, Reason: "EtcdClusterNotReady", Message: "DataStore is not reconciled until EtcdCluster is Ready"}, ec.Generation)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&ecv1alpha1.EtcdCluster{}).WithObjects(ec).Build()
	r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}
	local := &ecv1alpha1.EtcdCluster{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, local))
	setCondition(&local.Status.Conditions, metav1.Condition{Type: string(ecv1alpha1.DataStoreReady), Status: metav1.ConditionTrue, Reason: "DataStoreReady", Message: "DataStore hcp-etcd is reconciled"}, local.Generation)

	err := r.updateStatus(ctx, &reconcileState{cluster: local, sts: sts, memberListResp: members, memberHealth: health, dataStoreConditionSet: true})

	require.NoError(t, err)
	updated := &ecv1alpha1.EtcdCluster{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, updated))
	cond := conditionByType(updated.Status.Conditions, ecv1alpha1.DataStoreReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "DataStoreReady", cond.Reason)
	assert.Equal(t, "DataStore hcp-etcd is reconciled", cond.Message)
}

func TestUpdateStatusPreservesUnchangedDataStoreConditionSetDuringReconcile(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)
	ec := statusTestCluster(3)
	sts := statusTestStatefulSet(ec, 3)
	members := statusMemberList(&etcdserverpb.Member{ID: 1, Name: "etcd-0"}, &etcdserverpb.Member{ID: 2, Name: "etcd-1"}, &etcdserverpb.Member{ID: 3, Name: "etcd-2"})
	health := []etcdutils.EpHealth{epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, true, 1), epHealth("etcd-2", 3, true, 1)}
	ec.Status = buildClusterStatus(ec, sts, nil, members, health)
	cond := metav1.Condition{Type: string(ecv1alpha1.DataStoreReady), Status: metav1.ConditionTrue, Reason: "DataStoreReady", Message: "DataStore hcp-etcd is reconciled"}
	setCondition(&ec.Status.Conditions, cond, ec.Generation)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&ecv1alpha1.EtcdCluster{}).WithObjects(ec).Build()
	r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}
	local := &ecv1alpha1.EtcdCluster{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, local))
	state := &reconcileState{cluster: local, sts: sts, memberListResp: members, memberHealth: health}
	setDataStoreCondition(state, cond)

	err := r.updateStatus(ctx, state)

	require.NoError(t, err)
	updated := &ecv1alpha1.EtcdCluster{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, updated))
	updatedCond := conditionByType(updated.Status.Conditions, ecv1alpha1.DataStoreReady)
	require.NotNil(t, updatedCond)
	assert.Equal(t, metav1.ConditionTrue, updatedCond.Status)
	assert.Equal(t, "DataStoreReady", updatedCond.Reason)
}

func TestUpdateStatusDropsStaleDataStoreConditionWhenNotSetDuringReconcile(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)
	ec := statusTestCluster(3)
	sts := statusTestStatefulSet(ec, 3)
	members := statusMemberList(&etcdserverpb.Member{ID: 1, Name: "etcd-0"}, &etcdserverpb.Member{ID: 2, Name: "etcd-1"}, &etcdserverpb.Member{ID: 3, Name: "etcd-2"})
	health := []etcdutils.EpHealth{epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, true, 1), epHealth("etcd-2", 3, true, 1)}
	ec.Status = buildClusterStatus(ec, sts, nil, members, health)
	setCondition(&ec.Status.Conditions, metav1.Condition{Type: string(ecv1alpha1.DataStoreReady), Status: metav1.ConditionTrue, Reason: "DataStoreReady", Message: "stale"}, ec.Generation)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&ecv1alpha1.EtcdCluster{}).WithObjects(ec).Build()
	r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}

	err := r.updateStatus(ctx, &reconcileState{cluster: ec, sts: sts, memberListResp: members, memberHealth: health})

	require.NoError(t, err)
	updated := &ecv1alpha1.EtcdCluster{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: ec.Name, Namespace: ec.Namespace}, updated))
	assert.Nil(t, conditionByType(updated.Status.Conditions, ecv1alpha1.DataStoreReady))
}

func TestIsVersionUpgradeAllowedSetsConditionAndEventOnBlockedUpgrade(t *testing.T) {
	ctx := t.Context()
	ec := statusTestCluster(3)
	ec.Spec.Version = "3.4.0"
	sts := statusTestStatefulSet(ec, 3)
	sts.Spec.Template.Spec.Containers[0].Image = "registry/etcd:3.5.17"
	recorder := record.NewFakeRecorder(1)
	r := &EtcdClusterReconciler{Recorder: recorder}

	allowed := r.isVersionUpgradeAllowed(ctx, &reconcileState{cluster: ec, sts: sts})

	assert.False(t, allowed)
	cond := conditionByType(ec.Status.Conditions, ecv1alpha1.EtcdClusterReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "EtcdVersionUpgradeBlocked", cond.Reason)
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "EtcdVersionUpgradeBlocked")
	case <-time.After(time.Second):
		t.Fatal("expected warning event")
	}
}

func TestVersionUpgradeGuardAllowsPatchUpgrade(t *testing.T) {
	ec := statusTestCluster(3)
	ec.Spec.Version = "3.5.18"
	sts := statusTestStatefulSet(ec, 3)
	sts.Spec.Template.Spec.Containers[0].Image = "registry/etcd:3.5.17"
	r := &EtcdClusterReconciler{}

	allowed := r.isVersionUpgradeAllowed(t.Context(), &reconcileState{cluster: ec, sts: sts})

	assert.True(t, allowed)
	assert.Nil(t, conditionByType(ec.Status.Conditions, ecv1alpha1.EtcdClusterReady))
}

func TestVersionUpgradeGuardBlocksInvalidInitialVersion(t *testing.T) {
	ec := statusTestCluster(3)
	ec.Spec.Version = "not-a-version"
	r := &EtcdClusterReconciler{}

	allowed := r.isVersionUpgradeAllowed(t.Context(), &reconcileState{cluster: ec})

	assert.False(t, allowed)
	cond := conditionByType(ec.Status.Conditions, ecv1alpha1.EtcdClusterReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "EtcdVersionUpgradeBlocked", cond.Reason)
	assert.Contains(t, cond.Message, "desired version")
}

func TestVersionUpgradeGuardBlocksStatefulSetPatch(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)
	ec := statusTestCluster(3)
	ec.Spec.Version = "3.4.0"
	ec.Spec.Recovery = &ecv1alpha1.EtcdClusterRecoverySpec{Enabled: false}
	sts := statusTestStatefulSet(ec, 3)
	sts.Spec.Template.Spec.Containers[0].Image = "registry/etcd:3.5.17"
	members := statusMemberList(&etcdserverpb.Member{ID: 1, Name: "etcd-0"}, &etcdserverpb.Member{ID: 2, Name: "etcd-1"}, &etcdserverpb.Member{ID: 3, Name: "etcd-2"})
	health := []etcdutils.EpHealth{epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, true, 1), epHealth("etcd-2", 3, true, 1)}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ec, sts).Build()
	r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}

	res, err := r.reconcileClusterState(ctx, &reconcileState{cluster: ec, sts: sts, memberListResp: members, memberHealth: health})

	require.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
	updated := &appsv1.StatefulSet{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: sts.Name, Namespace: sts.Namespace}, updated))
	assert.Equal(t, "registry/etcd:3.5.17", updated.Spec.Template.Spec.Containers[0].Image)
}

func recoveryTestState(recoveryEnabled bool, health []etcdutils.EpHealth, pods []corev1.Pod) *reconcileState {
	recovery := &ecv1alpha1.EtcdClusterRecoverySpec{Enabled: recoveryEnabled, GracePeriod: &metav1.Duration{Duration: 0}}
	return &reconcileState{
		cluster: &ecv1alpha1.EtcdCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "etcd", Namespace: "default", UID: "recovery"},
			Spec: ecv1alpha1.EtcdClusterSpec{
				Size:    3,
				Version: "3.5.17",
				StorageSpec: &ecv1alpha1.StorageSpec{
					VolumeSizeRequest: resource.MustParse("1Gi"),
				},
				Recovery: recovery,
			},
		},
		sts: &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "etcd", Namespace: "default"},
			Spec:       appsv1.StatefulSetSpec{Replicas: pointerToInt32(3)},
			Status:     appsv1.StatefulSetStatus{Replicas: 3, ReadyReplicas: 2, UpdatedReplicas: 3, CurrentRevision: "rev", UpdateRevision: "rev"},
		},
		memberListResp: &clientv3.MemberListResponse{Members: []*etcdserverpb.Member{
			{ID: 1, Name: "etcd-0"},
			{ID: 2, Name: "etcd-1"},
			{ID: 3, Name: "etcd-2"},
		}},
		memberHealth: health,
		pods:         pods,
	}
}

func epHealth(name string, id uint64, healthy bool, leader uint64) etcdutils.EpHealth {
	return etcdutils.EpHealth{
		Ep:     "http://" + name + ".etcd.default.svc.cluster.local:2379",
		Health: healthy,
		Status: &clientv3.StatusResponse{
			Header: &etcdserverpb.ResponseHeader{MemberId: id},
			Leader: leader,
		},
	}
}

func etcdAlarm(memberID uint64, alarmType etcdserverpb.AlarmType) etcdutils.Alarm {
	return etcdutils.Alarm{MemberID: memberID, Type: alarmType}
}

func recoveryPod(name string, ready bool) corev1.Pod {
	condStatus := corev1.ConditionFalse
	if ready {
		condStatus = corev1.ConditionTrue
	}
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid")},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type:               corev1.PodReady,
			Status:             condStatus,
			LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
		}}},
	}
}

func TestRecoveryPreflightDisabledDoesNotTrigger(t *testing.T) {
	state := recoveryTestState(false, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1),
		epHealth("etcd-1", 2, false, 1),
		epHealth("etcd-2", 3, true, 1),
	}, nil)
	r := &EtcdClusterReconciler{}
	decision := r.evaluateRecoveryPreflight(state)
	assert.False(t, decision.HasUnhealthy)
	assert.False(t, decision.Allowed)
	assert.Equal(t, "RecoveryDisabled", decision.Reason)
}

func TestRecoveryPreflightBlocksQuorumLost(t *testing.T) {
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1),
		epHealth("etcd-1", 2, false, 1),
		epHealth("etcd-2", 3, false, 1),
	}, nil)
	r := &EtcdClusterReconciler{}
	decision := r.evaluateRecoveryPreflight(state)
	assert.True(t, decision.HasUnhealthy)
	assert.False(t, decision.Allowed)
	assert.Equal(t, ecv1alpha1.EtcdRecoveryResultBlocked, decision.Result)
	assert.Equal(t, "QuorumUnavailable", decision.Reason)
}

func TestRecoveryPreflightBlocksMultipleUnhealthyMembers(t *testing.T) {
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1),
		epHealth("etcd-1", 2, false, 1),
		epHealth("etcd-2", 3, false, 1),
	}, nil)
	// Keep quorum available while still observing multiple abnormal members.
	state.cluster.Spec.Size = 5
	state.sts.Spec.Replicas = pointerToInt32(5)
	state.sts.Status.UpdatedReplicas = 5
	state.memberListResp.Members = append(state.memberListResp.Members, &etcdserverpb.Member{ID: 4, Name: "etcd-3"}, &etcdserverpb.Member{ID: 5, Name: "etcd-4"})
	state.memberHealth = append(state.memberHealth, epHealth("etcd-3", 4, true, 1), epHealth("etcd-4", 5, true, 1))
	state.memberAlarms = []etcdutils.Alarm{etcdAlarm(2, etcdserverpb.AlarmType_CORRUPT), etcdAlarm(3, etcdserverpb.AlarmType_CORRUPT)}
	r := &EtcdClusterReconciler{}
	decision := r.evaluateRecoveryPreflight(state)
	assert.True(t, decision.HasUnhealthy)
	assert.False(t, decision.Allowed)
	assert.Equal(t, "MultipleUnhealthyMembers", decision.Reason)
}

func TestRecoveryDeletesOnlyTargetPodAndPVC(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)

	badPod := recoveryPod("etcd-1", false)
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1),
		epHealth("etcd-1", 2, false, 1),
		epHealth("etcd-2", 3, true, 1),
	}, []corev1.Pod{recoveryPod("etcd-0", true), badPod, recoveryPod("etcd-2", true)})
	state.memberAlarms = []etcdutils.Alarm{etcdAlarm(2, etcdserverpb.AlarmType_CORRUPT)}

	objs := []client.Object{
		state.cluster,
		&badPod,
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "etcd-0", Namespace: "default"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "etcd-2", Namespace: "default"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "etcd-data-etcd-0", Namespace: "default"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "etcd-data-etcd-1", Namespace: "default"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "etcd-data-etcd-2", Namespace: "default"}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}

	handled, res, err := r.reconcileSingleMemberRecovery(ctx, state)
	assert.True(t, handled)
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{RequeueAfter: requeueDuration}, res)
	require.NotNil(t, state.cluster.Status.Recovery)
	assert.Equal(t, ecv1alpha1.EtcdRecoveryResultRunning, state.cluster.Status.Recovery.LastResult)
	assert.Equal(t, "etcd-1", state.cluster.Status.Recovery.LastRecoveredMember)

	assert.True(t, apierrors.IsNotFound(fakeClient.Get(ctx, client.ObjectKey{Name: "etcd-1", Namespace: "default"}, &corev1.Pod{})))
	assert.True(t, apierrors.IsNotFound(fakeClient.Get(ctx, client.ObjectKey{Name: "etcd-data-etcd-1", Namespace: "default"}, &corev1.PersistentVolumeClaim{})))
	assert.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: "etcd-0", Namespace: "default"}, &corev1.Pod{}))
	assert.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: "etcd-data-etcd-0", Namespace: "default"}, &corev1.PersistentVolumeClaim{}))
	assert.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: "etcd-2", Namespace: "default"}, &corev1.Pod{}))
	assert.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: "etcd-data-etcd-2", Namespace: "default"}, &corev1.PersistentVolumeClaim{}))
}

func recoveryJobForTest(state *reconcileState, target string, attempt int32, condition batchv1.JobConditionType, creation time.Time) batchv1.Job {
	job := batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:              recoveryJobName(state.cluster, target, attempt),
		Namespace:         state.cluster.Namespace,
		Labels:            recoveryJobLabels(state.cluster, target, attempt),
		CreationTimestamp: metav1.NewTime(creation),
	}}
	if condition != "" {
		job.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue}}
	}
	return job
}

func TestOldCompletedRecoveryJobDoesNotBlockNewPodUID(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)

	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1),
	}, []corev1.Pod{recoveryPod("etcd-0", true), recoveryPod("etcd-1", false), recoveryPod("etcd-2", true)})
	state.memberAlarms = []etcdutils.Alarm{etcdAlarm(2, etcdserverpb.AlarmType_CORRUPT)}
	oldJob := recoveryJobForTest(state, "etcd-1", 1, batchv1.JobComplete, time.Now())
	oldJob.Annotations = map[string]string{"operator.etcd.io/recovery-target-pod-uid": "old-etcd-1-uid"}
	state.jobs = []batchv1.Job{oldJob}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "etcd-1", Namespace: "default", UID: "etcd-1-uid"}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "etcd-data-etcd-1", Namespace: "default"}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(state.cluster, &oldJob, pod, pvc).Build()
	r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}

	handled, res, err := r.reconcileSingleMemberRecovery(ctx, state)
	assert.True(t, handled)
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{RequeueAfter: requeueDuration}, res)
	created := &batchv1.Job{}
	assert.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: recoveryJobName(state.cluster, "etcd-1", 2), Namespace: "default"}, created))
	assert.Equal(t, "etcd-1-uid", created.Annotations["operator.etcd.io/recovery-target-pod-uid"])
	assert.True(t, apierrors.IsNotFound(fakeClient.Get(ctx, client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, &corev1.Pod{})))
}

func TestRecoveryJobRunningDoesNotRepeatDelete(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)

	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1),
	}, []corev1.Pod{recoveryPod("etcd-0", true), recoveryPod("etcd-1", false), recoveryPod("etcd-2", true)})
	state.memberAlarms = []etcdutils.Alarm{etcdAlarm(2, etcdserverpb.AlarmType_CORRUPT)}
	job := recoveryJobForTest(state, "etcd-1", 1, "", time.Now())
	state.jobs = []batchv1.Job{job}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "etcd-1", Namespace: "default"}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "etcd-data-etcd-1", Namespace: "default"}}
	r := &EtcdClusterReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(state.cluster, pod, pvc, &job).Build(), Scheme: scheme}

	handled, res, err := r.reconcileSingleMemberRecovery(ctx, state)
	assert.True(t, handled)
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{RequeueAfter: requeueDuration}, res)
	assert.Equal(t, ecv1alpha1.EtcdRecoveryResultRunning, state.cluster.Status.Recovery.LastResult)
	assert.NoError(t, r.Get(ctx, client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, &corev1.Pod{}))
	assert.NoError(t, r.Get(ctx, client.ObjectKey{Name: pvc.Name, Namespace: pvc.Namespace}, &corev1.PersistentVolumeClaim{}))
}

func TestRecoveryJobCompleteWaitsForValidationBeforeSucceeded(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1),
	}, nil)
	state.memberAlarms = []etcdutils.Alarm{etcdAlarm(2, etcdserverpb.AlarmType_CORRUPT)}
	job := recoveryJobForTest(state, "etcd-1", 1, batchv1.JobComplete, time.Now())
	state.jobs = []batchv1.Job{job}
	r := &EtcdClusterReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(state.cluster, &job).Build(), Scheme: scheme}

	handled, res, err := r.reconcileSingleMemberRecovery(ctx, state)
	assert.True(t, handled)
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{RequeueAfter: requeueDuration}, res)
	require.NotNil(t, state.cluster.Status.Recovery)
	assert.Equal(t, ecv1alpha1.EtcdRecoveryResultRunning, state.cluster.Status.Recovery.LastResult)
	assert.Equal(t, "RecoveryValidationPending", conditionByType(state.cluster.Status.Conditions, ecv1alpha1.SingleMemberRecoveryActive).Reason)
	assert.Equal(t, int32(1), state.cluster.Status.Recovery.RetryCount)
}

func TestRecoveryJobCompleteMarksSucceededAfterValidation(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, true, 1), epHealth("etcd-2", 3, true, 1),
	}, nil)
	state.sts.Status.ReadyReplicas = 3
	state.memberAlarms = []etcdutils.Alarm{etcdAlarm(2, etcdserverpb.AlarmType_CORRUPT)}
	job := recoveryJobForTest(state, "etcd-1", 1, batchv1.JobComplete, time.Now())
	state.jobs = []batchv1.Job{job}
	r := &EtcdClusterReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(state.cluster, &job).Build(), Scheme: scheme}

	handled, res, err := r.reconcileSingleMemberRecovery(ctx, state)
	assert.True(t, handled)
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, res)
	require.NotNil(t, state.cluster.Status.Recovery)
	assert.Equal(t, ecv1alpha1.EtcdRecoveryResultSucceeded, state.cluster.Status.Recovery.LastResult)
	assert.Equal(t, int32(1), state.cluster.Status.Recovery.RetryCount)
}

func TestRecoveryJobFailedCreatesRetryAttempt(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1),
	}, []corev1.Pod{recoveryPod("etcd-1", false)})
	state.memberAlarms = []etcdutils.Alarm{etcdAlarm(2, etcdserverpb.AlarmType_CORRUPT)}
	maxRetries := int32(2)
	state.cluster.Spec.Recovery.MaxRetries = &maxRetries
	failed := recoveryJobForTest(state, "etcd-1", 1, batchv1.JobFailed, time.Now())
	state.jobs = []batchv1.Job{failed}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "etcd-1", Namespace: "default"}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "etcd-data-etcd-1", Namespace: "default"}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(state.cluster, &failed, pod, pvc).Build()
	r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}

	handled, _, err := r.reconcileSingleMemberRecovery(ctx, state)
	assert.True(t, handled)
	assert.NoError(t, err)
	created := &batchv1.Job{}
	assert.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: recoveryJobName(state.cluster, "etcd-1", 2), Namespace: "default"}, created))
	assert.Equal(t, int32(2), state.cluster.Status.Recovery.RetryCount)
}

func TestRecoveryJobTimeoutMarksFailedWithoutDelete(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1),
	}, nil)
	state.memberAlarms = []etcdutils.Alarm{etcdAlarm(2, etcdserverpb.AlarmType_CORRUPT)}
	timeout := metav1.Duration{Duration: time.Second}
	state.cluster.Spec.Recovery.Timeout = &timeout
	job := recoveryJobForTest(state, "etcd-1", 1, "", time.Now().Add(-time.Hour))
	state.jobs = []batchv1.Job{job}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "etcd-1", Namespace: "default"}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "etcd-data-etcd-1", Namespace: "default"}}
	r := &EtcdClusterReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(state.cluster, &job, pod, pvc).Build(), Scheme: scheme}

	handled, _, err := r.reconcileSingleMemberRecovery(ctx, state)
	assert.True(t, handled)
	assert.NoError(t, err)
	assert.Equal(t, ecv1alpha1.EtcdRecoveryResultFailed, state.cluster.Status.Recovery.LastResult)
	assert.NoError(t, r.Get(ctx, client.ObjectKey{Name: pod.Name, Namespace: pod.Namespace}, &corev1.Pod{}))
	assert.NoError(t, r.Get(ctx, client.ObjectKey{Name: pvc.Name, Namespace: pvc.Namespace}, &corev1.PersistentVolumeClaim{}))
}

func TestRecoveryMaxRetriesBlocksNewAttempt(t *testing.T) {
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1),
	}, nil)
	maxRetries := int32(1)
	state.cluster.Spec.Recovery.MaxRetries = &maxRetries
	state.memberAlarms = []etcdutils.Alarm{etcdAlarm(2, etcdserverpb.AlarmType_CORRUPT)}
	state.cluster.Status.Recovery = &ecv1alpha1.EtcdClusterRecoveryStatus{LastRecoveredMember: "etcd-1", LastResult: ecv1alpha1.EtcdRecoveryResultFailed, RetryCount: 1}
	r := &EtcdClusterReconciler{}
	decision := r.evaluateRecoveryPreflight(state)
	assert.True(t, decision.HasUnhealthy)
	assert.False(t, decision.Allowed)
	assert.Equal(t, "MaxRetriesExceeded", decision.Reason)
}

func notReadyPodSince(name string, since time.Time) corev1.Pod {
	pod := recoveryPod(name, false)
	pod.Status.Conditions[0].LastTransitionTime = metav1.NewTime(since)
	return pod
}

func crashLoopPod(name string) corev1.Pod {
	pod := recoveryPod(name, true)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "etcd",
		RestartCount: 1,
		State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}}
	return pod
}

func TestRecoveryPreflightRequiresPersistentStorage(t *testing.T) {
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1),
	}, nil)
	state.cluster.Spec.StorageSpec = nil

	decision := (&EtcdClusterReconciler{}).evaluateRecoveryPreflight(state)

	assert.False(t, decision.Allowed)
	assert.Equal(t, ecv1alpha1.EtcdRecoveryResultBlocked, decision.Result)
	assert.Equal(t, "PersistentStorageRequired", decision.Reason)
}

func TestRecoveryPreflightBlocksScaleInProgress(t *testing.T) {
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1),
	}, nil)
	state.sts.Spec.Replicas = pointerToInt32(2)

	decision := (&EtcdClusterReconciler{}).evaluateRecoveryPreflight(state)

	assert.False(t, decision.Allowed)
	assert.Equal(t, "ScaleInProgress", decision.Reason)
}

func TestRecoveryPreflightBlocksMembershipChanging(t *testing.T) {
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1),
	}, nil)
	state.memberListResp.Members = state.memberListResp.Members[:2]

	decision := (&EtcdClusterReconciler{}).evaluateRecoveryPreflight(state)

	assert.False(t, decision.Allowed)
	assert.Equal(t, "MembershipChanging", decision.Reason)
}

func TestRecoveryPreflightBlocksRollingUpdateInProgress(t *testing.T) {
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1),
	}, nil)
	state.sts.Status.CurrentRevision = "old"
	state.sts.Status.UpdateRevision = "new"

	decision := (&EtcdClusterReconciler{}).evaluateRecoveryPreflight(state)

	assert.False(t, decision.Allowed)
	assert.Equal(t, "RollingUpdateInProgress", decision.Reason)
}

func TestRecoveryPreflightBlocksLearnerPresent(t *testing.T) {
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1),
	}, nil)
	state.memberListResp.Members[2].IsLearner = true

	decision := (&EtcdClusterReconciler{}).evaluateRecoveryPreflight(state)

	assert.True(t, decision.HasUnhealthy)
	assert.False(t, decision.Allowed)
	assert.Equal(t, "LearnerPresent", decision.Reason)
}

func TestRecoveryPreflightWaitsForGracePeriod(t *testing.T) {
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1),
	}, []corev1.Pod{notReadyPodSince("etcd-1", time.Now().Add(-time.Minute))})
	state.memberAlarms = []etcdutils.Alarm{etcdAlarm(2, etcdserverpb.AlarmType_CORRUPT)}
	state.cluster.Spec.Recovery.GracePeriod = &metav1.Duration{Duration: 10 * time.Minute}

	decision := (&EtcdClusterReconciler{}).evaluateRecoveryPreflight(state)

	assert.True(t, decision.HasUnhealthy)
	assert.False(t, decision.Allowed)
	assert.Equal(t, ecv1alpha1.EtcdRecoveryResultPending, decision.Result)
	assert.Equal(t, "GracePeriodNotElapsed", decision.Reason)
	assert.Equal(t, "etcd-1", decision.TargetPod)
	assert.Equal(t, "etcd-data-etcd-1", decision.TargetPVC)
}

func TestRecoveryPreflightAllowsAfterGracePeriod(t *testing.T) {
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1),
	}, []corev1.Pod{notReadyPodSince("etcd-1", time.Now().Add(-time.Hour))})
	state.memberAlarms = []etcdutils.Alarm{etcdAlarm(2, etcdserverpb.AlarmType_CORRUPT)}
	state.cluster.Spec.Recovery.GracePeriod = &metav1.Duration{Duration: 10 * time.Minute}

	decision := (&EtcdClusterReconciler{}).evaluateRecoveryPreflight(state)

	assert.True(t, decision.Allowed)
	assert.Equal(t, ecv1alpha1.EtcdRecoveryResultRunning, decision.Result)
	assert.Equal(t, "CorruptAlarm", decision.Reason)
	assert.Equal(t, "etcd-1", decision.TargetMember)
}

func TestRecoveryPreflightBlocksNoSpaceAlarm(t *testing.T) {
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1),
	}, nil)
	state.memberAlarms = []etcdutils.Alarm{etcdAlarm(2, etcdserverpb.AlarmType_NOSPACE)}

	decision := (&EtcdClusterReconciler{}).evaluateRecoveryPreflight(state)

	assert.True(t, decision.HasUnhealthy)
	assert.False(t, decision.Allowed)
	assert.Equal(t, ecv1alpha1.EtcdRecoveryResultBlocked, decision.Result)
	assert.Equal(t, "NoSpaceAlarm", decision.Reason)
}

func TestRecoveryPreflightCrashLoopWithoutCorruptAlarmDoesNotRecover(t *testing.T) {
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, true, 1), epHealth("etcd-2", 3, true, 1),
	}, []corev1.Pod{crashLoopPod("etcd-1")})

	decision := (&EtcdClusterReconciler{}).evaluateRecoveryPreflight(state)

	assert.True(t, decision.HasUnhealthy)
	assert.False(t, decision.Allowed)
	assert.Equal(t, ecv1alpha1.EtcdRecoveryResultPending, decision.Result)
	assert.Equal(t, "NoCorruptAlarm", decision.Reason)
}

func TestCreateRecoveryJobMetadata(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)
	state := recoveryTestState(true, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, false, 1), epHealth("etcd-2", 3, true, 1),
	}, []corev1.Pod{recoveryPod("etcd-1", false)})
	state.cluster.Generation = 9
	decision := recoveryDecision{Allowed: true, TargetMember: "etcd-1", TargetPod: "etcd-1", TargetPVC: "etcd-data-etcd-1", Reason: "SingleUnhealthyMember"}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(state.cluster).Build()
	r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}

	job, err := r.createRecoveryJob(ctx, state, decision, 2)

	require.NoError(t, err)
	assert.Equal(t, "etcd-recover-1-2", job.Name)
	assert.Equal(t, "true", job.Labels["operator.etcd.io/recovery-job"])
	assert.Equal(t, "etcd-1", job.Labels["operator.etcd.io/recovery-target"])
	assert.Equal(t, "2", job.Labels["operator.etcd.io/recovery-attempt"])
	assert.Equal(t, "etcd-1", job.Annotations["operator.etcd.io/recovery-target-pod"])
	assert.Equal(t, "etcd-data-etcd-1", job.Annotations["operator.etcd.io/recovery-target-pvc"])
	assert.Equal(t, "etcd-1-uid", job.Annotations["operator.etcd.io/recovery-target-pod-uid"])
	assert.Equal(t, "9", job.Annotations["operator.etcd.io/cluster-generation"])
	assert.Equal(t, "SingleUnhealthyMember", job.Annotations["operator.etcd.io/preflight-reason"])
	assert.Equal(t, "true", job.Annotations["operator.etcd.io/preflight-confirmed"])
	assert.Equal(t, "false", job.Annotations["operator.etcd.io/destructive-executed"])
	require.NotNil(t, job.Spec.BackoffLimit)
	assert.Equal(t, int32(0), *job.Spec.BackoffLimit)
	require.NotNil(t, job.Spec.TTLSecondsAfterFinished)
	assert.Equal(t, int32(86400), *job.Spec.TTLSecondsAfterFinished)
	assert.Equal(t, job.Labels, job.Spec.Template.Labels)
	require.Len(t, job.OwnerReferences, 1)
	assert.Equal(t, state.cluster.Name, job.OwnerReferences[0].Name)
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	assert.Contains(t, job.Spec.Template.Spec.Containers[0].Args[0], "controller deleted pod etcd-1 and pvc etcd-data-etcd-1")
}

func readyDataStoreState(annotation string) *reconcileState {
	state := recoveryTestState(false, []etcdutils.EpHealth{
		epHealth("etcd-0", 1, true, 1), epHealth("etcd-1", 2, true, 1), epHealth("etcd-2", 3, true, 1),
	}, nil)
	state.cluster.Annotations = map[string]string{}
	if annotation != "" {
		state.cluster.Annotations[dataStoreAnnotation] = annotation
	}
	state.cluster.Spec.TLS = &ecv1alpha1.TLSCertificate{Provider: "cert-manager", ProviderCfg: ecv1alpha1.ProviderConfig{CertManagerCfg: &ecv1alpha1.ProviderCertManagerConfig{IssuerKind: "Issuer", IssuerName: "etcd-ca-issuer"}}}
	state.sts.Status.ReadyReplicas = 3
	return state
}

func dataStoreTLSObjects(namespace string) []client.Object {
	return []client.Object{
		&certv1.Issuer{ObjectMeta: metav1.ObjectMeta{Name: "etcd-ca-issuer", Namespace: namespace}, Spec: certv1.IssuerSpec{IssuerConfig: certv1.IssuerConfig{CA: &certv1.CAIssuer{SecretName: "etcd-root-ca"}}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "etcd-root-ca", Namespace: namespace}, Data: map[string][]byte{corev1.TLSCertKey: []byte("ca"), corev1.TLSPrivateKeyKey: []byte("key")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: getClientCertName("etcd"), Namespace: namespace}, Data: map[string][]byte{corev1.TLSCertKey: []byte("client"), corev1.TLSPrivateKeyKey: []byte("key")}},
	}
}

func TestDataStoreNoAnnotationNoop(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = ecv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}
	state := readyDataStoreState("")
	state.cluster.Status.Conditions = []metav1.Condition{
		{Type: string(ecv1alpha1.DataStoreReady), Status: metav1.ConditionTrue, Reason: "DataStoreReady"},
		{Type: string(ecv1alpha1.EtcdClusterReady), Status: metav1.ConditionTrue, Reason: "ClusterReady"},
	}

	assert.NoError(t, r.reconcileDataStore(ctx, state))
	require.Len(t, state.cluster.Status.Conditions, 1)
	assert.Equal(t, string(ecv1alpha1.EtcdClusterReady), state.cluster.Status.Conditions[0].Type)
}

func TestDataStoreNotReadyDoesNotCreate(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = ecv1alpha1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}
	state := readyDataStoreState("branch-a-etcd")
	state.sts.Status.ReadyReplicas = 2

	assert.NoError(t, r.reconcileDataStore(ctx, state))
	var cond *metav1.Condition
	for i := range state.cluster.Status.Conditions {
		if state.cluster.Status.Conditions[i].Type == string(ecv1alpha1.DataStoreReady) {
			cond = &state.cluster.Status.Conditions[i]
		}
	}
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "EtcdClusterNotReady", cond.Reason)
}

func TestDataStoreCreateAndUpdateWhenReady(t *testing.T) {
	ctx := t.Context()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = certv1.AddToScheme(scheme)
	_ = ecv1alpha1.AddToScheme(scheme)
	state := readyDataStoreState("branch-a-etcd")
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dataStoreTLSObjects(state.cluster.Namespace)...).Build()
	r := &EtcdClusterReconciler{Client: fakeClient, Scheme: scheme}

	assert.NoError(t, r.reconcileDataStore(ctx, state))
	created := &unstructured.Unstructured{}
	created.SetGroupVersionKind(dataStoreGVK)
	assert.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: "branch-a-etcd"}, created))
	assert.Equal(t, "etcd", created.Object["spec"].(map[string]any)["driver"])
	assert.Equal(t, []any{"etcd-client.default.svc:2379"}, created.Object["spec"].(map[string]any)["endpoints"])
	assert.Nil(t, created.Object["spec"].(map[string]any)["basicAuth"])
	tlsConfig := created.Object["spec"].(map[string]any)["tlsConfig"].(map[string]any)
	ca := tlsConfig["certificateAuthority"].(map[string]any)
	clientCert := tlsConfig["clientCertificate"].(map[string]any)
	caCertRef := ca["certificate"].(map[string]any)["secretReference"].(map[string]any)
	caKeyRef := ca["privateKey"].(map[string]any)["secretReference"].(map[string]any)
	clientCertRef := clientCert["certificate"].(map[string]any)["secretReference"].(map[string]any)
	clientKeyRef := clientCert["privateKey"].(map[string]any)["secretReference"].(map[string]any)
	assert.Equal(t, "etcd-root-ca", caCertRef["name"])
	assert.Equal(t, "default", caCertRef["namespace"])
	assert.Equal(t, "tls.crt", caCertRef["keyPath"])
	assert.Equal(t, "etcd-root-ca", caKeyRef["name"])
	assert.Equal(t, "tls.key", caKeyRef["keyPath"])
	assert.Equal(t, getClientCertName(state.cluster.Name), clientCertRef["name"])
	assert.Equal(t, "tls.crt", clientCertRef["keyPath"])
	assert.Equal(t, getClientCertName(state.cluster.Name), clientKeyRef["name"])
	assert.Equal(t, "tls.key", clientKeyRef["keyPath"])

	// Mutate and ensure reconcile updates spec back to desired.
	created.Object["spec"] = map[string]any{"driver": "memory"}
	assert.NoError(t, fakeClient.Update(ctx, created))
	assert.NoError(t, r.reconcileDataStore(ctx, state))
	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(dataStoreGVK)
	assert.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: "branch-a-etcd"}, updated))
	assert.Equal(t, "etcd", updated.Object["spec"].(map[string]any)["driver"])
}

func TestDataStoreCRDAbsentNoop(t *testing.T) {
	ctx := t.Context()
	r := &EtcdClusterReconciler{Client: datastoreNoMatchClient{}, Scheme: runtime.NewScheme()}
	state := readyDataStoreState("branch-a-etcd")
	assert.NoError(t, r.reconcileDataStore(ctx, state))
	require.Len(t, state.cluster.Status.Conditions, 1)
	assert.Equal(t, string(ecv1alpha1.DataStoreReady), state.cluster.Status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionFalse, state.cluster.Status.Conditions[0].Status)
	assert.Equal(t, "DataStoreCRDNotFound", state.cluster.Status.Conditions[0].Reason)
}

type datastoreNoMatchClient struct{ client.Client }

func (datastoreNoMatchClient) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return &apimeta.NoKindMatchError{GroupKind: dataStoreGVK.GroupKind()}
}
