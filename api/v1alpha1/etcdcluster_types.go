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

package v1alpha1

import (
	"net"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// EtcdClusterSpec defines the desired state of EtcdCluster.
type EtcdClusterSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Size is the expected size of the etcd cluster.
	// +kubebuilder:validation:Minimum=1
	Size int `json:"size"`
	// ImageRegistry specifies the container registry that hosts the etcd images.
	// If unset, it defaults to the value provided via the controller's
	// --image-registry flag, which itself defaults to "gcr.io/etcd-development/etcd".
	ImageRegistry string `json:"imageRegistry,omitempty"`
	// Version is the expected version of the etcd container image.
	// It must be an etcd semantic version such as "3.5.17" or "v3.5.17".
	// +kubebuilder:validation:Pattern=`^v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`
	Version string `json:"version"`
	// StorageSpec is the name of the StorageSpec to use for the etcd cluster. If not provided, then each POD just uses the temporary storage inside the container.
	StorageSpec *StorageSpec `json:"storageSpec,omitempty"`
	// TLS is the TLS certificate configuration to use for the etcd cluster and etcd operator.
	TLS *TLSCertificate `json:"tls,omitempty"`
	// etcd configuration options are passed as command line arguments to the etcd container, refer to etcd documentation for configuration options applicable for the version of etcd being used.
	EtcdOptions []string `json:"etcdOptions,omitempty"`
	// PodTemplate is the pod template to use for the etcd cluster.
	PodTemplate *PodTemplate `json:"podTemplate,omitempty"`
	// Recovery configures automatic single-member recovery behavior for the etcd cluster.
	// When unset or disabled, the controller will not perform automatic recovery actions.
	Recovery *EtcdClusterRecoverySpec `json:"recovery,omitempty"`
}

type PodTemplate struct {
	// Metadata is the metadata to add to the pod.
	Metadata *PodMetadata `json:"metadata,omitempty"`
	// Spec contains the scheduling-related fields to apply to the etcd pods.
	Spec *EtcdPodTemplateSpec `json:"spec,omitempty"`
}

// EtcdPodTemplateSpec contains the supported pod spec overrides for etcd pods.
type EtcdPodTemplateSpec struct {
	// NodeSelector is a selector which must be true for the pod to fit on a node.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations are appended to the pod to allow scheduling onto nodes with matching taints.
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// Affinity specifies the pod's scheduling constraints.
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
	// TopologySpreadConstraints describes how the pods should be spread across topology domains.
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	// PriorityClassName indicates the pod's priority class.
	PriorityClassName string `json:"priorityClassName,omitempty"`
}

type PodMetadata struct {
	// Annotations are added to the generated etcd pods.
	Annotations map[string]string `json:"annotations,omitempty"`
	// Labels are added to the generated etcd pods.
	Labels map[string]string `json:"labels,omitempty"`
}

// EtcdClusterRecoverySpec configures automatic single-member recovery.
type EtcdClusterRecoverySpec struct {
	// Enabled controls whether automatic single-member recovery is allowed.
	Enabled bool `json:"enabled,omitempty"`
	// GracePeriod is the amount of time a member must remain unhealthy before recovery may start.
	GracePeriod *metav1.Duration `json:"gracePeriod,omitempty"`
	// Timeout is the maximum amount of time a single recovery attempt may run.
	Timeout *metav1.Duration `json:"timeout,omitempty"`
	// MaxRetries is the maximum number of recovery attempts for the same failed member.
	// +kubebuilder:validation:Minimum=0
	MaxRetries *int32 `json:"maxRetries,omitempty"`
}

type TLSCertificate struct {
	Provider    string         `json:"provider,omitempty"` // Defaults to Auto provider if not present
	ProviderCfg ProviderConfig `json:"providerCfg,omitempty"`
}

type ProviderConfig struct {
	AutoCfg        *ProviderAutoConfig        `json:"autoCfg,omitempty"`
	CertManagerCfg *ProviderCertManagerConfig `json:"certManagerCfg,omitempty"`
}

type AltNames struct {
	// DNSNames is the expected array of DNS subject alternative names.
	// if empty defaults to $(POD_NAME).$(ETCD_CLUSTER_NAME).$(POD_NAMESPACE).svc.cluster.local
	// +optional
	DNSNames []string `json:"dnsNames,omitempty"`

	// IPs is the expected array of IP address subject alternative names.
	// +optional
	IPs []net.IP `json:"ipAddresses,omitempty"`
}

type CommonConfig struct {
	// CommonName is the expected common name X509 certificate subject attribute.
	// Should have a length of 64 characters or fewer to avoid generating invalid CSRs.
	// +optional
	CommonName string `json:"commonName,omitempty"`

	// Organization is the expected array of Organization names to be used on the Certificate.
	// +optional
	Organization []string `json:"organizations,omitempty"`

	// AltNames contains the domain names and IP addresses that will be added
	// to the x509 certificate SubAltNames fields. The values will be passed
	// directly to the x509.Certificate object.
	AltNames AltNames `json:"altNames,omitempty"`

	// ValidityDuration is the expected duration until which the certificate will be valid,
	// expects in human-readable duration: 100d12h, if empty defaults to 90d
	// +optional
	ValidityDuration string `json:"validityDuration,omitempty"`

	// CABundleSecret is the expected secret name with CABundle present. It's used
	// by each etcd POD to verify TLS communications with its peers or clients. If it isn't
	// provided, the CA included in the secret generated by certificate provider will be
	// used instead if present; otherwise, there is no way to verify TLS communications.
	// +optional
	CABundleSecret string `json:"caBundleSecret,omitempty"`
}

type ProviderAutoConfig struct {
	// CommonConfig is the struct of common fields required to create a certificate
	CommonConfig `json:",inline"`
}

type ProviderCertManagerConfig struct {
	// CommonConfig is the struct of common fields required to create a certificate
	CommonConfig `json:",inline"`

	// IssuerKind is the expected kind of Issuer, either "ClusterIssuer" or "Issuer"
	IssuerKind string `json:"issuerKind"`

	// IssuerName is the expected name of Issuer required to issue a certificate
	IssuerName string `json:"issuerName"`
}

// EtcdClusterPhase describes the high-level observed phase of an EtcdCluster.
type EtcdClusterPhase string

const (
	// EtcdClusterPhasePending indicates the cluster has not become ready yet.
	EtcdClusterPhasePending EtcdClusterPhase = "Pending"
	// EtcdClusterPhaseReady indicates the cluster is ready and has quorum.
	EtcdClusterPhaseReady EtcdClusterPhase = "Ready"
	// EtcdClusterPhaseDegraded indicates the cluster is running with reduced health or capacity.
	EtcdClusterPhaseDegraded EtcdClusterPhase = "Degraded"
	// EtcdClusterPhaseRecovering indicates the cluster has an active recovery operation.
	EtcdClusterPhaseRecovering EtcdClusterPhase = "Recovering"
	// EtcdClusterPhaseFailed indicates the cluster cannot make progress without intervention.
	EtcdClusterPhaseFailed EtcdClusterPhase = "Failed"
)

// EtcdClusterConditionType is a condition type reported on an EtcdCluster.
type EtcdClusterConditionType string

const (
	// EtcdClusterCreated indicates the base Kubernetes resources for the cluster have been created.
	EtcdClusterCreated EtcdClusterConditionType = "EtcdClusterCreated"
	// EtcdClusterReady indicates the cluster is ready to serve client traffic.
	EtcdClusterReady EtcdClusterConditionType = "EtcdClusterReady"
	// QuorumAvailable indicates enough healthy voting members are available to maintain quorum.
	QuorumAvailable EtcdClusterConditionType = "QuorumAvailable"
	// SingleMemberRecoveryActive indicates an automatic single-member recovery operation is in progress.
	SingleMemberRecoveryActive EtcdClusterConditionType = "SingleMemberRecoveryActive"
	// DataStoreReady indicates the optional external DataStore integration is ready.
	DataStoreReady EtcdClusterConditionType = "DataStoreReady"
)

// EtcdClusterStatus defines the observed state of EtcdCluster.
type EtcdClusterStatus struct {
	// Phase is a high-level summary of the current cluster state.
	// +kubebuilder:validation:Enum=Pending;Ready;Degraded;Recovering;Failed
	Phase EtcdClusterPhase `json:"phase,omitempty"`
	// ReadyReplicas is the number of etcd pods currently considered ready.
	// +kubebuilder:validation:Minimum=0
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// MemberCount is the number of members currently observed in the etcd membership list.
	// +kubebuilder:validation:Minimum=0
	MemberCount int32 `json:"memberCount,omitempty"`
	// LeaderID is the etcd member ID of the current leader, when known.
	LeaderID string `json:"leaderID,omitempty"`
	// Members contains the observed status of each etcd member.
	// +listType=map
	// +listMapKey=name
	Members []EtcdMemberStatus `json:"members,omitempty"`
	// Recovery contains the latest observed single-member recovery state.
	Recovery *EtcdClusterRecoveryStatus `json:"recovery,omitempty"`
	// Conditions represent the latest available observations of the cluster state.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedGeneration is the most recent generation observed by the controller.
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// EtcdMemberStatus describes an observed etcd member.
type EtcdMemberStatus struct {
	// Name is the Kubernetes pod/member name associated with this etcd member.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// ID is the etcd member ID.
	ID string `json:"id,omitempty"`
	// Healthy indicates whether the member passed the latest health check.
	Healthy bool `json:"healthy,omitempty"`
	// Leader indicates whether this member is the current etcd leader.
	Leader bool `json:"leader,omitempty"`
	// Learner indicates whether this member is an etcd learner and not a voting member.
	Learner bool `json:"learner,omitempty"`
	// NodeName is the Kubernetes node currently running the member pod, when known.
	NodeName string `json:"nodeName,omitempty"`
}

// EtcdRecoveryResult describes the state/result of a single-member recovery attempt.
type EtcdRecoveryResult string

const (
	// EtcdRecoveryResultPending indicates recovery is waiting for safety preconditions or grace period.
	EtcdRecoveryResultPending EtcdRecoveryResult = "Pending"
	// EtcdRecoveryResultRunning indicates recovery destructive actions have been started.
	EtcdRecoveryResultRunning EtcdRecoveryResult = "Running"
	// EtcdRecoveryResultSucceeded indicates the last recovery attempt succeeded.
	EtcdRecoveryResultSucceeded EtcdRecoveryResult = "Succeeded"
	// EtcdRecoveryResultFailed indicates the last recovery attempt failed.
	EtcdRecoveryResultFailed EtcdRecoveryResult = "Failed"
	// EtcdRecoveryResultBlocked indicates recovery was blocked by safety preconditions.
	EtcdRecoveryResultBlocked EtcdRecoveryResult = "Blocked"
)

// EtcdClusterRecoveryStatus describes observed single-member recovery state.
type EtcdClusterRecoveryStatus struct {
	// LastResult is the result of the most recent recovery attempt.
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Blocked
	LastResult EtcdRecoveryResult `json:"lastResult,omitempty"`
	// LastRecoveredMember is the member name targeted by the most recent recovery attempt.
	LastRecoveredMember string `json:"lastRecoveredMember,omitempty"`
	// LastTransitionTime is the time the recovery status last changed.
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
	// RetryCount is the number of recovery attempts made for the current failed member.
	// +kubebuilder:validation:Minimum=0
	RetryCount int32 `json:"retryCount,omitempty"`
	// Message provides human-readable details about the latest recovery state.
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 52",message="metadata.name must be no more than 52 characters to keep generated resource names within the Kubernetes 63-character limit"

// EtcdCluster is the Schema for the etcdclusters API.
type EtcdCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EtcdClusterSpec   `json:"spec,omitempty"`
	Status EtcdClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EtcdClusterList contains a list of EtcdCluster.
type EtcdClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EtcdCluster `json:"items"`
}

type StorageSpec struct {
	AccessModes       corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`      // `ReadWriteOnce` (default) or `ReadWriteMany`. Note that `ReadOnlyMany` isn't allowed.
	StorageClassName  string                            `json:"storageClassName,omitempty"` // optional, the default one will be used if not specified
	PVCName           string                            `json:"pvcName,omitempty"`          // optional, only used when access mode is ReadWriteMany
	VolumeSizeRequest resource.Quantity                 `json:"volumeSizeRequest"`          // required.
	VolumeSizeLimit   resource.Quantity                 `json:"volumeSizeLimit,omitempty"`  // optional
}

func init() {
	SchemeBuilder.Register(&EtcdCluster{}, &EtcdClusterList{})
}
