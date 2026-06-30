package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEtcdClusterDeepCopySeparatesPodTemplateMapsAndSlices(t *testing.T) {
	original := &EtcdCluster{
		Spec: EtcdClusterSpec{
			PodTemplate: &PodTemplate{
				Metadata: &PodMetadata{
					Labels:      map[string]string{"app": "etcd"},
					Annotations: map[string]string{"owner": "operator"},
				},
				Spec: &EtcdPodTemplateSpec{
					NodeSelector: map[string]string{"hcp": "true"},
					Tolerations:  []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "hcp"}},
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
						MaxSkew:           1,
						TopologyKey:       "kubernetes.io/hostname",
						WhenUnsatisfiable: corev1.DoNotSchedule,
					}},
					PriorityClassName: "system-cluster-critical",
				},
			},
		},
	}

	copy := original.DeepCopy()
	copy.Spec.PodTemplate.Metadata.Labels["app"] = "changed"
	copy.Spec.PodTemplate.Metadata.Annotations["owner"] = "changed"
	copy.Spec.PodTemplate.Spec.NodeSelector["hcp"] = "false"
	copy.Spec.PodTemplate.Spec.Tolerations[0].Value = "changed"
	copy.Spec.PodTemplate.Spec.TopologySpreadConstraints[0].TopologyKey = "zone"
	copy.Spec.PodTemplate.Spec.PriorityClassName = "custom"

	assert.Equal(t, "etcd", original.Spec.PodTemplate.Metadata.Labels["app"])
	assert.Equal(t, "operator", original.Spec.PodTemplate.Metadata.Annotations["owner"])
	assert.Equal(t, "true", original.Spec.PodTemplate.Spec.NodeSelector["hcp"])
	assert.Equal(t, "hcp", original.Spec.PodTemplate.Spec.Tolerations[0].Value)
	assert.Equal(t, "kubernetes.io/hostname", original.Spec.PodTemplate.Spec.TopologySpreadConstraints[0].TopologyKey)
	assert.Equal(t, "system-cluster-critical", original.Spec.PodTemplate.Spec.PriorityClassName)
}

func TestEtcdClusterDeepCopySeparatesRecoverySpec(t *testing.T) {
	grace := metav1.Duration{Duration: 10 * time.Minute}
	timeout := metav1.Duration{Duration: 30 * time.Minute}
	maxRetries := int32(3)
	original := &EtcdCluster{Spec: EtcdClusterSpec{Recovery: &EtcdClusterRecoverySpec{
		Enabled:     true,
		GracePeriod: &grace,
		Timeout:     &timeout,
		MaxRetries:  &maxRetries,
	}}}

	copy := original.DeepCopy()
	copy.Spec.Recovery.GracePeriod.Duration = time.Minute
	copy.Spec.Recovery.Timeout.Duration = 2 * time.Minute
	*copy.Spec.Recovery.MaxRetries = 1
	copy.Spec.Recovery.Enabled = false

	assert.True(t, original.Spec.Recovery.Enabled)
	assert.Equal(t, 10*time.Minute, original.Spec.Recovery.GracePeriod.Duration)
	assert.Equal(t, 30*time.Minute, original.Spec.Recovery.Timeout.Duration)
	assert.Equal(t, int32(3), *original.Spec.Recovery.MaxRetries)
}

func TestEtcdClusterDeepCopySeparatesStatusMembersConditionsRecovery(t *testing.T) {
	now := metav1.NewTime(time.Unix(100, 0))
	original := &EtcdCluster{Status: EtcdClusterStatus{
		Members: []EtcdMemberStatus{{Name: "etcd-0", ID: "1", Healthy: true}},
		Conditions: []metav1.Condition{{
			Type:               string(EtcdClusterReady),
			Status:             metav1.ConditionTrue,
			Reason:             "ClusterReady",
			LastTransitionTime: now,
		}},
		Recovery: &EtcdClusterRecoveryStatus{
			LastResult:          EtcdRecoveryResultRunning,
			LastRecoveredMember: "etcd-1",
			LastTransitionTime:  &now,
			RetryCount:          1,
			Message:             "running",
		},
	}}

	copy := original.DeepCopy()
	copy.Status.Members[0].Name = "changed"
	copy.Status.Conditions[0].Reason = "Changed"
	copy.Status.Recovery.LastResult = EtcdRecoveryResultSucceeded
	copy.Status.Recovery.LastTransitionTime.Time = time.Unix(200, 0)

	assert.Equal(t, "etcd-0", original.Status.Members[0].Name)
	assert.Equal(t, "ClusterReady", original.Status.Conditions[0].Reason)
	assert.Equal(t, EtcdRecoveryResultRunning, original.Status.Recovery.LastResult)
	assert.Equal(t, time.Unix(100, 0), original.Status.Recovery.LastTransitionTime.Time)
}

func TestEtcdClusterJSONRoundTripIncludesHCPFields(t *testing.T) {
	grace := metav1.Duration{Duration: 5 * time.Minute}
	maxRetries := int32(2)
	original := EtcdCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "etcd"},
		Spec: EtcdClusterSpec{
			Size:    3,
			Version: "3.5.17",
			PodTemplate: &PodTemplate{Spec: &EtcdPodTemplateSpec{
				NodeSelector:      map[string]string{"hcp": "true"},
				PriorityClassName: "system-cluster-critical",
			}},
			Recovery: &EtcdClusterRecoverySpec{Enabled: true, GracePeriod: &grace, MaxRetries: &maxRetries},
		},
		Status: EtcdClusterStatus{
			Phase:              EtcdClusterPhaseReady,
			ReadyReplicas:      3,
			MemberCount:        3,
			LeaderID:           "1",
			ObservedGeneration: 2,
			Members:            []EtcdMemberStatus{{Name: "etcd-0", Healthy: true, Leader: true, NodeName: "node-0"}},
			Recovery:           &EtcdClusterRecoveryStatus{LastResult: EtcdRecoveryResultSucceeded, LastRecoveredMember: "etcd-1"},
			Conditions:         []metav1.Condition{{Type: string(EtcdClusterReady), Status: metav1.ConditionTrue, Reason: "ClusterReady"}},
		},
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)
	var decoded EtcdCluster
	require.NoError(t, json.Unmarshal(data, &decoded))

	assert.Equal(t, "true", decoded.Spec.PodTemplate.Spec.NodeSelector["hcp"])
	assert.Equal(t, "system-cluster-critical", decoded.Spec.PodTemplate.Spec.PriorityClassName)
	assert.True(t, decoded.Spec.Recovery.Enabled)
	assert.Equal(t, 5*time.Minute, decoded.Spec.Recovery.GracePeriod.Duration)
	assert.Equal(t, int32(2), *decoded.Spec.Recovery.MaxRetries)
	assert.Equal(t, EtcdClusterPhaseReady, decoded.Status.Phase)
	assert.Equal(t, "node-0", decoded.Status.Members[0].NodeName)
	assert.Equal(t, EtcdRecoveryResultSucceeded, decoded.Status.Recovery.LastResult)
}
