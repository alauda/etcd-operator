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
	"crypto/tls"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	certv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ecv1alpha1 "go.etcd.io/etcd-operator/api/v1alpha1"
	"go.etcd.io/etcd-operator/internal/etcdutils"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	requeueDuration     = 10 * time.Second
	dataStoreAnnotation = "hcp.alauda.io/datastore"
)

var dataStoreGVK = schema.GroupVersionKind{Group: "kamaji.clastix.io", Version: "v1alpha1", Kind: "DataStore"}

// EtcdClusterReconciler reconciles a EtcdCluster object
type EtcdClusterReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Recorder      record.EventRecorder
	ImageRegistry string
	ProbeImage    string
}

// reconcileState holds all transient data for a single reconciliation loop.
// Every phase of Reconcile stores intermediate information here so that
// subsequent phases can operate without additional lookups.
type reconcileState struct {
	cluster                   *ecv1alpha1.EtcdCluster      // cluster custom resource currently being reconciled
	sts                       *appsv1.StatefulSet          // associated StatefulSet for the cluster
	tlsConfig                 *tls.Config                  // TLS configuration for the etcd cluster
	memberListResp            *clientv3.MemberListResponse // member list fetched from the etcd cluster
	memberHealth              []etcdutils.EpHealth         // health information for each etcd member
	memberAlarms              []etcdutils.Alarm            // alarms observed from etcd AlarmList
	alarmListErr              error                        // AlarmList error; recovery fails closed when set
	pods                      []corev1.Pod                 // pods owned by the StatefulSet
	jobs                      []batchv1.Job                // recovery jobs owned by the cluster
	recoveryConditionSet      bool                         // true when this reconcile directly set the recovery condition
	dataStoreConditionSet     bool                         // true when this reconcile directly set the DataStore condition
	dataStoreConditionChanged bool                         // true when this reconcile changed the DataStore condition
	readyGuardConditionSet    bool                         // true when this reconcile directly set a Ready guard condition
}

// +kubebuilder:rbac:groups=operator.etcd.io,resources=etcdclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=operator.etcd.io,resources=etcdclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=operator.etcd.io,resources=etcdclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kamaji.clastix.io,resources=datastores,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch;get;list;update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups="cert-manager.io",resources=certificates,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups="cert-manager.io",resources=clusterissuers,verbs=get;list;watch
// +kubebuilder:rbac:groups="cert-manager.io",resources=issuers,verbs=get;list;watch

// Reconcile orchestrates a single reconciliation cycle for an EtcdCluster. It
// sequentially fetches resources, ensures primitive objects exist, checks the
// health of the etcd cluster and then adjusts its state to match the desired
// specification. Each phase is handled by a dedicated helper method.
//
// For more details on the controller-runtime Reconcile contract see:
// https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/reconcile
func (r *EtcdClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	state, res, err := r.fetchAndValidateState(ctx, req)
	if state == nil || err != nil {
		return res, err
	}

	defer func() {
		if updateErr := r.updateStatus(ctx, state); updateErr != nil {
			log.FromContext(ctx).Error(updateErr, "Failed to update EtcdCluster status")
		}
	}()

	// Block invalid or unsafe version changes before any path can create or patch
	// the StatefulSet template image, including initial bootstrap, bootstrap-from-zero,
	// membership drift repair, scale, or the steady-state patch path.
	if !r.isVersionUpgradeAllowed(ctx, state) {
		return ctrl.Result{}, nil
	}

	if bootstrapRes, err := r.bootstrapStatefulSet(ctx, state); err != nil || !bootstrapRes.IsZero() {
		return bootstrapRes, err
	}

	if err = r.performHealthChecks(ctx, state); err != nil {
		return ctrl.Result{}, err
	}

	res, err = r.reconcileClusterState(ctx, state)
	if err != nil || !res.IsZero() {
		return res, err
	}

	if err := r.reconcileDataStore(ctx, state); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// fetchAndValidateState retrieves the EtcdCluster and its StatefulSet and ensures
// the StatefulSet, if present, is owned by the cluster. It returns a populated
// reconcileState for use in later phases. A non-empty ctrl.Result requests a
// requeue when transient issues occur.
func (r *EtcdClusterReconciler) fetchAndValidateState(ctx context.Context, req ctrl.Request) (*reconcileState, ctrl.Result, error) {
	logger := log.FromContext(ctx)

	ec := &ecv1alpha1.EtcdCluster{}
	if err := r.Get(ctx, req.NamespacedName, ec); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("EtcdCluster resource not found. Ignoring since object may have been deleted")
			return nil, ctrl.Result{}, nil
		}
		return nil, ctrl.Result{}, err
	}

	// Determine desired etcd image registry
	if ec.Spec.ImageRegistry == "" {
		ec.Spec.ImageRegistry = r.ImageRegistry
	}

	// Ensure the operator has TLS credentials when the cluster requests TLS.
	if ec.Spec.TLS != nil {
		if err := createClientCertificate(ctx, ec, r.Client); err != nil {
			logger.Error(err, "Failed to create Client Certificate.")
		}
	} else {
		// TODO: instead of logging error, set default autoConfig
		logger.Error(nil, fmt.Sprintf(
			"missing TLS config for %s,\n running etcd-cluster without TLS protection is NOT recommended for production.",
			ec.Name,
		))
	}

	logger.Info("Reconciling EtcdCluster", "spec", ec.Spec)

	sts, err := getStatefulSet(ctx, r.Client, ec.Name, ec.Namespace)
	if err != nil {
		if errors.IsNotFound(err) {
			sts = nil
		} else {
			logger.Error(err, "Failed to get StatefulSet. Requesting requeue")
			return nil, ctrl.Result{RequeueAfter: requeueDuration}, nil
		}
	}

	var pods []corev1.Pod
	var jobs []batchv1.Job
	if sts != nil {
		if err := checkStatefulSetControlledByEtcdOperator(ec, sts); err != nil {
			logger.Error(err, "StatefulSet is not controlled by this EtcdCluster resource")
			return nil, ctrl.Result{}, err
		}
		podList := &corev1.PodList{}
		if err := r.List(ctx, podList, client.InNamespace(ec.Namespace), client.MatchingLabels(labelsForEtcdCluster(ec))); err != nil {
			logger.Error(err, "Failed to list etcd pods. Requesting requeue")
			return nil, ctrl.Result{RequeueAfter: requeueDuration}, nil
		}
		pods = podList.Items

		jobList := &batchv1.JobList{}
		if err := r.List(ctx, jobList, client.InNamespace(ec.Namespace), client.MatchingLabels(recoveryJobBaseLabels(ec))); err != nil {
			logger.Error(err, "Failed to list recovery jobs. Requesting requeue")
			return nil, ctrl.Result{RequeueAfter: requeueDuration}, nil
		}
		jobs = jobList.Items
	}

	tlsConfig, err := getTlsConfig(ctx, r.Client, ec)
	if err != nil {
		return nil, ctrl.Result{}, err
	}

	return &reconcileState{cluster: ec, sts: sts, tlsConfig: tlsConfig, pods: pods, jobs: jobs}, ctrl.Result{}, nil
}

func (r *EtcdClusterReconciler) reconcileStatefulSet(ctx context.Context, logger logr.Logger, ec *ecv1alpha1.EtcdCluster, replicas int32) (*appsv1.StatefulSet, error) {
	return reconcileStatefulSet(ctx, logger, ec, r.Client, replicas, r.Scheme, StatefulSetOptions{ProbeImage: r.ProbeImage})
}

func normalizeEtcdVersion(v string) (major, minor, patch int, err error) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if idx := strings.IndexAny(v, "-+"); idx >= 0 {
		v = v[:idx]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, 0, 0, fmt.Errorf("invalid etcd version %q", v)
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return
	}
	if len(parts) == 3 {
		patch, err = strconv.Atoi(parts[2])
	}
	return
}

func validateEtcdVersionUpgrade(current, desired string) error {
	curMajor, curMinor, curPatch, err := normalizeEtcdVersion(current)
	if err != nil {
		return fmt.Errorf("current version: %w", err)
	}
	desiredMajor, desiredMinor, desiredPatch, err := normalizeEtcdVersion(desired)
	if err != nil {
		return fmt.Errorf("desired version: %w", err)
	}
	if desiredMajor != curMajor {
		return fmt.Errorf("major version change from %s to %s is not supported", current, desired)
	}
	if desiredMinor < curMinor || (desiredMinor == curMinor && desiredPatch < curPatch) {
		return fmt.Errorf("downgrade from %s to %s is not supported", current, desired)
	}
	if desiredMinor-curMinor > 1 {
		return fmt.Errorf("cross-minor upgrade from %s to %s is not supported", current, desired)
	}
	return nil
}

func etcdContainerVersion(sts *appsv1.StatefulSet) string {
	if sts == nil {
		return ""
	}
	for _, c := range sts.Spec.Template.Spec.Containers {
		if c.Name == "etcd" {
			idx := strings.LastIndex(c.Image, ":")
			if idx >= 0 && idx+1 < len(c.Image) {
				return c.Image[idx+1:]
			}
		}
	}
	return ""
}

func (r *EtcdClusterReconciler) isVersionUpgradeAllowed(ctx context.Context, s *reconcileState) bool {
	current := etcdContainerVersion(s.sts)
	desired := s.cluster.Spec.Version
	if _, _, _, err := normalizeEtcdVersion(desired); err != nil {
		err = fmt.Errorf("desired version: %w", err)
		setCondition(&s.cluster.Status.Conditions, metav1.Condition{
			Type:    string(ecv1alpha1.EtcdClusterReady),
			Status:  metav1.ConditionFalse,
			Reason:  "EtcdVersionUpgradeBlocked",
			Message: err.Error(),
		}, s.cluster.Generation)
		s.readyGuardConditionSet = true
		if r.Recorder != nil {
			r.Recorder.Event(s.cluster, corev1.EventTypeWarning, "EtcdVersionUpgradeBlocked", err.Error())
		}
		log.FromContext(ctx).Info("Blocked invalid etcd version", "desired", desired, "reason", err.Error())
		return false
	}
	if current == "" || current == desired {
		return true
	}
	if err := validateEtcdVersionUpgrade(current, desired); err != nil {
		setCondition(&s.cluster.Status.Conditions, metav1.Condition{
			Type:    string(ecv1alpha1.EtcdClusterReady),
			Status:  metav1.ConditionFalse,
			Reason:  "EtcdVersionUpgradeBlocked",
			Message: err.Error(),
		}, s.cluster.Generation)
		s.readyGuardConditionSet = true
		if r.Recorder != nil {
			r.Recorder.Event(s.cluster, corev1.EventTypeWarning, "EtcdVersionUpgradeBlocked", err.Error())
		}
		log.FromContext(ctx).Info("Blocked unsafe etcd version change", "current", current, "desired", desired, "reason", err.Error())
		return false
	}
	return true
}

// bootstrapStatefulSet ensures that the foundational Kubernetes objects for
// a cluster exist and are correctly initialized. It creates the StatefulSet (initially
// with 0 replicas) and the headless Service if necessary. When either resource
// is created or the StatefulSet is scaled from zero to one replica, the returned
// ctrl.Result requests a requeue so the next reconciliation loop can observe the
// new state. The reconcileState is updated with the current StatefulSet.
func (r *EtcdClusterReconciler) bootstrapStatefulSet(ctx context.Context, s *reconcileState) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	requeue := false
	var err error

	if s.sts != nil && s.sts.Spec.PodManagementPolicy != appsv1.ParallelPodManagement {
		message := "existing StatefulSet uses immutable podManagementPolicy OrderedReady; migrate by safely recreating the StatefulSet with podManagementPolicy Parallel while preserving PVCs"
		setCondition(&s.cluster.Status.Conditions, metav1.Condition{Type: string(ecv1alpha1.EtcdClusterReady), Status: metav1.ConditionFalse, Reason: "StatefulSetPodManagementPolicyNeedsMigration", Message: message}, s.cluster.Generation)
		s.readyGuardConditionSet = true
		if r.Recorder != nil {
			r.Recorder.Event(s.cluster, corev1.EventTypeWarning, "StatefulSetPodManagementPolicyNeedsMigration", message)
		}
	}

	switch {
	case s.sts == nil:
		logger.Info("Creating StatefulSet with 0 replica", "expectedSize", s.cluster.Spec.Size)
		s.sts, err = r.reconcileStatefulSet(ctx, logger, s.cluster, 0)
		if err != nil {
			return ctrl.Result{}, err
		}
		requeue = true

	case s.sts.Spec.Replicas != nil && *s.sts.Spec.Replicas == 0:
		logger.Info("StatefulSet has 0 replicas. Trying to create a new cluster with 1 member")
		s.sts, err = r.reconcileStatefulSet(ctx, logger, s.cluster, 1)
		if err != nil {
			return ctrl.Result{}, err
		}
		requeue = true
	}

	if err = createHeadlessServiceIfNotExist(ctx, logger, r.Client, s.cluster, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err = createClientServiceIfNotExist(ctx, logger, r.Client, s.cluster, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if err = createOrPatchPodDisruptionBudget(ctx, logger, r.Client, s.cluster, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	if requeue {
		return ctrl.Result{RequeueAfter: requeueDuration}, nil
	}
	return ctrl.Result{}, nil
}

// performHealthChecks obtains the member list and health status from the etcd
// cluster specified in the StatefulSet. Results are stored on the reconcileState
// for later reconciliation steps.
func (r *EtcdClusterReconciler) performHealthChecks(ctx context.Context, s *reconcileState) error {
	logger := log.FromContext(ctx)
	logger.Info("Now checking health of the cluster members")
	var err error
	s.memberListResp, s.memberHealth, err = healthCheck(s.sts, logger, s.tlsConfig)
	if s.memberListResp != nil {
		s.memberAlarms, s.alarmListErr = etcdutils.AlarmList(clientEndpointsFromStatefulsets(s.sts, s.tlsConfig), s.tlsConfig)
		if s.alarmListErr != nil {
			logger.Info("Failed to list etcd alarms; recovery preflight will fail closed", "error", s.alarmListErr.Error())
		}
	}
	if err != nil {
		if s.memberListResp != nil && len(s.memberHealth) > 0 {
			logger.Info("Health check observed unhealthy members; continuing so recovery preflight can evaluate", "error", err.Error())
			return nil
		}
		return fmt.Errorf("health check failed: %w", err)
	}
	return nil
}

type recoveryDecision struct {
	HasUnhealthy bool
	Allowed      bool
	TargetMember string
	TargetPod    string
	TargetPVC    string
	Result       ecv1alpha1.EtcdRecoveryResult
	Reason       string
	Message      string
}

func defaultRecoveryGracePeriod(recovery *ecv1alpha1.EtcdClusterRecoverySpec) time.Duration {
	if recovery != nil && recovery.GracePeriod != nil {
		return recovery.GracePeriod.Duration
	}
	return 10 * time.Minute
}

func maxRecoveryRetries(recovery *ecv1alpha1.EtcdClusterRecoverySpec) int32 {
	if recovery != nil && recovery.MaxRetries != nil {
		return *recovery.MaxRetries
	}
	return 3
}

func podNameForEndpoint(ec *ecv1alpha1.EtcdCluster, endpoint string) string {
	for i := 0; i < ec.Spec.Size; i++ {
		name := fmt.Sprintf("%s-%d", ec.Name, i)
		if strings.Contains(endpoint, name+".") || strings.Contains(endpoint, name+":") {
			return name
		}
	}
	return ""
}

func pvcNameForPod(podName string) string {
	return fmt.Sprintf("%s-%s", volumeName, podName)
}

func recoveryOrdinalFromPodName(ec *ecv1alpha1.EtcdCluster, podName string) string {
	prefix := ec.Name + "-"
	return strings.TrimPrefix(podName, prefix)
}

func recoveryJobBaseLabels(ec *ecv1alpha1.EtcdCluster) map[string]string {
	return map[string]string{
		"app":                           ec.Name,
		"controller":                    ec.Name,
		"operator.etcd.io/recovery-job": "true",
	}
}

func recoveryJobLabels(ec *ecv1alpha1.EtcdCluster, target string, attempt int32) map[string]string {
	labels := recoveryJobBaseLabels(ec)
	labels["operator.etcd.io/recovery-target"] = target
	labels["operator.etcd.io/recovery-attempt"] = strconv.FormatInt(int64(attempt), 10)
	return labels
}

func recoveryJobName(ec *ecv1alpha1.EtcdCluster, target string, attempt int32) string {
	ordinal := recoveryOrdinalFromPodName(ec, target)
	return fmt.Sprintf("%s-recover-%s-%d", ec.Name, ordinal, attempt)
}

func recoveryJobAttempt(job batchv1.Job) int32 {
	if job.Labels == nil {
		return 0
	}
	attempt, _ := strconv.ParseInt(job.Labels["operator.etcd.io/recovery-attempt"], 10, 32)
	return int32(attempt)
}

func targetPodUID(pods []corev1.Pod, podName string) string {
	for _, pod := range pods {
		if pod.Name == podName {
			return string(pod.UID)
		}
	}
	return ""
}

func isJobComplete(job batchv1.Job) bool {
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isJobFailed(job batchv1.Job) bool {
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func activeRecoveryJobForTarget(jobs []batchv1.Job, target, podUID string) *batchv1.Job {
	var latest *batchv1.Job
	for i := range jobs {
		job := &jobs[i]
		if job.Labels["operator.etcd.io/recovery-target"] != target {
			continue
		}
		if podUID != "" {
			if jobPodUID, ok := job.Annotations["operator.etcd.io/recovery-target-pod-uid"]; ok && jobPodUID != podUID {
				continue
			}
		}
		if latest == nil || recoveryJobAttempt(*job) > recoveryJobAttempt(*latest) {
			latest = job
		}
	}
	return latest
}

func recoveryTimeout(recovery *ecv1alpha1.EtcdClusterRecoverySpec) time.Duration {
	if recovery != nil && recovery.Timeout != nil {
		return recovery.Timeout.Duration
	}
	return 30 * time.Minute
}

func isPodCrashLooping(pod corev1.Pod) bool {
	for _, status := range pod.Status.ContainerStatuses {
		if status.RestartCount > 0 && status.State.Waiting != nil && status.State.Waiting.Reason == "CrashLoopBackOff" {
			return true
		}
	}
	return false
}

func podNotReadySince(pod corev1.Pod) *metav1.Time {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status != corev1.ConditionTrue {
			return &cond.LastTransitionTime
		}
	}
	return nil
}

func isStatefulSetRollingUpdate(sts *appsv1.StatefulSet) bool {
	if sts == nil || sts.Spec.Replicas == nil {
		return false
	}
	if sts.Status.UpdateRevision != "" && sts.Status.CurrentRevision != "" && sts.Status.UpdateRevision != sts.Status.CurrentRevision {
		return true
	}
	return sts.Status.UpdatedReplicas > 0 && sts.Status.UpdatedReplicas < *sts.Spec.Replicas
}

func memberNameByID(members []*etcdserverpb.Member, id uint64) string {
	for _, member := range members {
		if member.ID == id {
			return member.Name
		}
	}
	return ""
}

func alarmMembersByType(members []*etcdserverpb.Member, alarms []etcdutils.Alarm, alarmType etcdserverpb.AlarmType) map[string]string {
	result := map[string]string{}
	for _, alarm := range alarms {
		if alarm.Type != alarmType {
			continue
		}
		name := memberNameByID(members, alarm.MemberID)
		if name == "" {
			name = strconv.FormatUint(alarm.MemberID, 10)
		}
		result[name] = alarmType.String()
	}
	return result
}

func recoveryGraceElapsed(ec *ecv1alpha1.EtcdCluster, target string, pods []corev1.Pod, grace time.Duration) bool {
	if grace <= 0 {
		return true
	}
	if ec.Status.Recovery != nil && ec.Status.Recovery.LastRecoveredMember == target && ec.Status.Recovery.LastResult == ecv1alpha1.EtcdRecoveryResultPending && ec.Status.Recovery.LastTransitionTime != nil {
		if time.Since(ec.Status.Recovery.LastTransitionTime.Time) >= grace {
			return true
		}
	}
	for _, pod := range pods {
		if pod.Name != target {
			continue
		}
		transition := podNotReadySince(pod)
		return transition != nil && time.Since(transition.Time) >= grace
	}
	return false
}

func (r *EtcdClusterReconciler) evaluateRecoveryPreflight(s *reconcileState) recoveryDecision {
	ec := s.cluster
	if ec.Spec.Recovery == nil || !ec.Spec.Recovery.Enabled {
		return recoveryDecision{Reason: "RecoveryDisabled", Message: "automatic single-member recovery is disabled"}
	}
	if s.sts == nil || s.sts.Spec.Replicas == nil {
		return recoveryDecision{Result: ecv1alpha1.EtcdRecoveryResultBlocked, Reason: "StatefulSetUnavailable", Message: "StatefulSet is not available"}
	}
	if ec.Spec.StorageSpec == nil {
		return recoveryDecision{Result: ecv1alpha1.EtcdRecoveryResultBlocked, Reason: "PersistentStorageRequired", Message: "recovery requires a per-member persistent volume"}
	}
	if int(*s.sts.Spec.Replicas) != ec.Spec.Size {
		return recoveryDecision{Result: ecv1alpha1.EtcdRecoveryResultBlocked, Reason: "ScaleInProgress", Message: "StatefulSet replica count does not match spec.size"}
	}
	if s.memberListResp == nil || len(s.memberListResp.Members) != ec.Spec.Size {
		return recoveryDecision{Result: ecv1alpha1.EtcdRecoveryResultBlocked, Reason: "MembershipChanging", Message: "etcd member count does not match spec.size"}
	}
	if isStatefulSetRollingUpdate(s.sts) {
		return recoveryDecision{Result: ecv1alpha1.EtcdRecoveryResultBlocked, Reason: "RollingUpdateInProgress", Message: "StatefulSet rolling update is in progress"}
	}

	healthByMemberName := map[string]etcdutils.EpHealth{}
	observedUnhealthy := map[string]string{}
	healthyVoting := 0
	learnerPresent := false
	for _, member := range s.memberListResp.Members {
		if member.IsLearner {
			learnerPresent = true
		}
		for _, h := range s.memberHealth {
			if h.Status != nil && h.Status.Header != nil && h.Status.Header.MemberId == member.ID {
				healthByMemberName[member.Name] = h
				break
			}
		}
	}
	for _, h := range s.memberHealth {
		if h.Status != nil && h.Status.IsLearner {
			learnerPresent = true
		}
		name := ""
		if h.Status != nil && h.Status.Header != nil {
			for _, member := range s.memberListResp.Members {
				if member.ID == h.Status.Header.MemberId {
					name = member.Name
					break
				}
			}
		}
		if name == "" {
			name = podNameForEndpoint(ec, h.Ep)
		}
		if h.Health && h.Status != nil && !h.Status.IsLearner {
			healthyVoting++
		}
		if name != "" && (!h.Health || h.Status == nil) {
			observedUnhealthy[name] = "member health check failed"
		}
	}
	for _, member := range s.memberListResp.Members {
		if _, ok := healthByMemberName[member.Name]; !ok {
			observedUnhealthy[member.Name] = "member endpoint health is missing"
		}
	}
	for _, pod := range s.pods {
		if isPodCrashLooping(pod) {
			observedUnhealthy[pod.Name] = "pod is CrashLoopBackOff"
		}
	}

	noSpaceMembers := alarmMembersByType(s.memberListResp.Members, s.memberAlarms, etcdserverpb.AlarmType_NOSPACE)
	corruptMembers := alarmMembersByType(s.memberListResp.Members, s.memberAlarms, etcdserverpb.AlarmType_CORRUPT)
	maps.Copy(observedUnhealthy, noSpaceMembers)
	maps.Copy(observedUnhealthy, corruptMembers)
	hasUnhealthy := len(observedUnhealthy) > 0

	if len(noSpaceMembers) > 0 {
		return recoveryDecision{HasUnhealthy: true, Result: ecv1alpha1.EtcdRecoveryResultBlocked, Reason: "NoSpaceAlarm", Message: "NOSPACE alarms require compact/defrag/disarm and are not handled by automatic single-member recovery"}
	}
	if s.alarmListErr != nil && hasUnhealthy {
		return recoveryDecision{HasUnhealthy: true, Result: ecv1alpha1.EtcdRecoveryResultBlocked, Reason: "AlarmListUnavailable", Message: fmt.Sprintf("cannot confirm CORRUPT alarm for automatic recovery: %v", s.alarmListErr)}
	}
	if learnerPresent {
		return recoveryDecision{HasUnhealthy: hasUnhealthy, Result: ecv1alpha1.EtcdRecoveryResultBlocked, Reason: "LearnerPresent", Message: "recovery is blocked while a learner member exists"}
	}
	quorum := healthyVoting >= ec.Spec.Size/2+1
	if !quorum {
		return recoveryDecision{HasUnhealthy: hasUnhealthy, Result: ecv1alpha1.EtcdRecoveryResultBlocked, Reason: "QuorumUnavailable", Message: "quorum is unavailable; automatic recovery is blocked"}
	}
	if !hasUnhealthy {
		return recoveryDecision{Reason: "NoUnhealthyMember", Message: "no unhealthy member observed"}
	}
	if len(corruptMembers) == 0 {
		return recoveryDecision{HasUnhealthy: true, Result: ecv1alpha1.EtcdRecoveryResultPending, Reason: "NoCorruptAlarm", Message: "automatic recovery waits for a single CORRUPT alarm; endpoint health issues are observation-only"}
	}
	if len(corruptMembers) > 1 {
		names := make([]string, 0, len(corruptMembers))
		for name := range corruptMembers {
			names = append(names, name)
		}
		sort.Strings(names)
		return recoveryDecision{HasUnhealthy: true, Result: ecv1alpha1.EtcdRecoveryResultBlocked, Reason: "MultipleUnhealthyMembers", Message: fmt.Sprintf("multiple members have CORRUPT alarms: %s", strings.Join(names, ","))}
	}

	target := ""
	for name := range corruptMembers {
		target = name
	}
	grace := defaultRecoveryGracePeriod(ec.Spec.Recovery)
	if grace > 0 && !recoveryGraceElapsed(ec, target, s.pods, grace) {
		return recoveryDecision{HasUnhealthy: true, TargetMember: target, TargetPod: target, TargetPVC: pvcNameForPod(target), Result: ecv1alpha1.EtcdRecoveryResultPending, Reason: "GracePeriodNotElapsed", Message: fmt.Sprintf("waiting for recovery grace period %s for %s", grace, target)}
	}
	if ec.Status.Recovery != nil && ec.Status.Recovery.LastRecoveredMember == target && ec.Status.Recovery.LastResult != ecv1alpha1.EtcdRecoveryResultRunning && ec.Status.Recovery.RetryCount >= maxRecoveryRetries(ec.Spec.Recovery) {
		return recoveryDecision{HasUnhealthy: true, TargetMember: target, TargetPod: target, TargetPVC: pvcNameForPod(target), Result: ecv1alpha1.EtcdRecoveryResultBlocked, Reason: "MaxRetriesExceeded", Message: fmt.Sprintf("recovery retry count for %s reached maxRetries", target)}
	}
	return recoveryDecision{HasUnhealthy: true, Allowed: true, TargetMember: target, TargetPod: target, TargetPVC: pvcNameForPod(target), Result: ecv1alpha1.EtcdRecoveryResultRunning, Reason: "CorruptAlarm", Message: fmt.Sprintf("single member %s has CORRUPT alarm", target)}
}

func setRecoveryCondition(ec *ecv1alpha1.EtcdCluster, active bool, reason, message string) {
	setCondition(&ec.Status.Conditions, metav1.Condition{
		Type:    string(ecv1alpha1.SingleMemberRecoveryActive),
		Status:  boolConditionStatus(active),
		Reason:  reason,
		Message: message,
	}, ec.Generation)
}

func (r *EtcdClusterReconciler) setRecoveryStatus(s *reconcileState, result ecv1alpha1.EtcdRecoveryResult, member, message string, active bool, reason string) {
	ec := s.cluster
	retryCount := int32(0)
	if ec.Status.Recovery != nil && ec.Status.Recovery.LastRecoveredMember == member {
		retryCount = ec.Status.Recovery.RetryCount
		if result == ecv1alpha1.EtcdRecoveryResultRunning && ec.Status.Recovery.LastResult != ecv1alpha1.EtcdRecoveryResultRunning {
			retryCount++
		}
	} else if result == ecv1alpha1.EtcdRecoveryResultRunning {
		retryCount = 1
	}
	now := metav1.Now()
	ec.Status.Recovery = &ecv1alpha1.EtcdClusterRecoveryStatus{
		LastResult:          result,
		LastRecoveredMember: member,
		LastTransitionTime:  &now,
		RetryCount:          retryCount,
		Message:             message,
	}
	if active {
		ec.Status.Phase = ecv1alpha1.EtcdClusterPhaseRecovering
	}
	setRecoveryCondition(ec, active, reason, message)
	s.recoveryConditionSet = true
}

func (r *EtcdClusterReconciler) createRecoveryJob(ctx context.Context, s *reconcileState, decision recoveryDecision, attempt int32) (*batchv1.Job, error) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: recoveryJobName(s.cluster, decision.TargetMember, attempt), Namespace: s.cluster.Namespace}}
	labels := recoveryJobLabels(s.cluster, decision.TargetMember, attempt)
	backoffLimit := int32(0)
	ttl := int32(24 * 60 * 60)
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, job, func() error {
		job.Labels = labels
		job.Annotations = map[string]string{
			"operator.etcd.io/recovery-target-pod":  decision.TargetPod,
			"operator.etcd.io/recovery-target-pvc":  decision.TargetPVC,
			"operator.etcd.io/cluster-generation":   strconv.FormatInt(s.cluster.Generation, 10),
			"operator.etcd.io/preflight-reason":     decision.Reason,
			"operator.etcd.io/preflight-confirmed":  "true",
			"operator.etcd.io/destructive-executed": "false",
		}
		if podUID := targetPodUID(s.pods, decision.TargetPod); podUID != "" {
			job.Annotations["operator.etcd.io/recovery-target-pod-uid"] = podUID
		}
		job.Spec.BackoffLimit = &backoffLimit
		job.Spec.TTLSecondsAfterFinished = &ttl
		job.Spec.Template.ObjectMeta.Labels = labels
		job.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
		image := etcdImageForEtcdCluster(s.cluster)
		if s.cluster.Spec.ImageRegistry == "" {
			image = "busybox:1.36"
		}
		job.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:    "record-recovery",
			Image:   image,
			Command: []string{"/bin/sh", "-c"},
			Args:    []string{fmt.Sprintf("echo recovery preflight confirmed for %s attempt %d; echo controller deleted pod %s and pvc %s", decision.TargetMember, attempt, decision.TargetPod, decision.TargetPVC)},
		}}
		return controllerutil.SetControllerReference(s.cluster, job, r.Scheme)
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

func (r *EtcdClusterReconciler) markRecoveryJobDestructiveExecuted(ctx context.Context, job *batchv1.Job) error {
	original := job.DeepCopy()
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations["operator.etcd.io/destructive-executed"] = "true"
	return r.Patch(ctx, job, client.MergeFrom(original))
}

func recoverySucceeded(s *reconcileState) bool {
	if s == nil || s.cluster == nil || s.sts == nil || s.memberListResp == nil {
		return false
	}
	if int(s.sts.Status.ReadyReplicas) != s.cluster.Spec.Size || len(s.memberListResp.Members) != s.cluster.Spec.Size || len(s.memberHealth) != s.cluster.Spec.Size {
		return false
	}
	for _, h := range s.memberHealth {
		if !h.Health || h.Status == nil || h.Status.IsLearner {
			return false
		}
	}
	return true
}

func (r *EtcdClusterReconciler) observeRecoveryJob(ctx context.Context, s *reconcileState, decision recoveryDecision) (bool, ctrl.Result, error) {
	if decision.TargetMember == "" {
		return false, ctrl.Result{}, nil
	}
	job := activeRecoveryJobForTarget(s.jobs, decision.TargetMember, targetPodUID(s.pods, decision.TargetPod))
	if job == nil {
		return false, ctrl.Result{}, nil
	}
	attempt := recoveryJobAttempt(*job)
	if isJobFailed(*job) {
		r.setRecoveryStatus(s, ecv1alpha1.EtcdRecoveryResultFailed, decision.TargetMember, fmt.Sprintf("recovery job %s failed", job.Name), false, "RecoveryJobFailed")
		if s.cluster.Status.Recovery != nil {
			s.cluster.Status.Recovery.RetryCount = attempt
		}
		return attempt >= maxRecoveryRetries(s.cluster.Spec.Recovery), ctrl.Result{RequeueAfter: requeueDuration}, nil
	}
	if timeout := recoveryTimeout(s.cluster.Spec.Recovery); timeout > 0 && time.Since(job.CreationTimestamp.Time) > timeout {
		r.setRecoveryStatus(s, ecv1alpha1.EtcdRecoveryResultFailed, decision.TargetMember, fmt.Sprintf("recovery job %s timed out after %s", job.Name, timeout), false, "RecoveryTimedOut")
		if s.cluster.Status.Recovery != nil {
			s.cluster.Status.Recovery.RetryCount = attempt
		}
		return true, ctrl.Result{RequeueAfter: requeueDuration}, nil
	}
	if job.Annotations["operator.etcd.io/destructive-executed"] == "false" {
		if err := r.deleteRecoveryTarget(ctx, s, decision); err != nil {
			r.setRecoveryStatus(s, ecv1alpha1.EtcdRecoveryResultFailed, decision.TargetMember, err.Error(), false, "RecoveryDeleteFailed")
			if s.cluster.Status.Recovery != nil {
				s.cluster.Status.Recovery.RetryCount = attempt
			}
			return true, ctrl.Result{}, err
		}
		if err := r.markRecoveryJobDestructiveExecuted(ctx, job); err != nil {
			r.setRecoveryStatus(s, ecv1alpha1.EtcdRecoveryResultFailed, decision.TargetMember, err.Error(), false, "RecoveryJobPatchFailed")
			if s.cluster.Status.Recovery != nil {
				s.cluster.Status.Recovery.RetryCount = attempt
			}
			return true, ctrl.Result{}, err
		}
		r.setRecoveryStatus(s, ecv1alpha1.EtcdRecoveryResultRunning, decision.TargetMember, fmt.Sprintf("resumed recovery job %s and deleted Pod %s/PVC %s", job.Name, decision.TargetPod, decision.TargetPVC), true, "RecoveryJobRunning")
		if s.cluster.Status.Recovery != nil {
			s.cluster.Status.Recovery.RetryCount = attempt
		}
		return true, ctrl.Result{RequeueAfter: requeueDuration}, nil
	}
	if isJobComplete(*job) {
		if recoverySucceeded(s) {
			r.setRecoveryStatus(s, ecv1alpha1.EtcdRecoveryResultSucceeded, decision.TargetMember, fmt.Sprintf("recovery job %s completed and cluster health validation passed", job.Name), false, "RecoverySucceeded")
			if s.cluster.Status.Recovery != nil {
				s.cluster.Status.Recovery.RetryCount = attempt
			}
			return true, ctrl.Result{}, nil
		}
		r.setRecoveryStatus(s, ecv1alpha1.EtcdRecoveryResultRunning, decision.TargetMember, fmt.Sprintf("recovery job %s completed; waiting for Pod Ready, member count, endpoint health, and learner checks", job.Name), true, "RecoveryValidationPending")
		if s.cluster.Status.Recovery != nil {
			s.cluster.Status.Recovery.RetryCount = attempt
		}
		return true, ctrl.Result{RequeueAfter: requeueDuration}, nil
	}
	r.setRecoveryStatus(s, ecv1alpha1.EtcdRecoveryResultRunning, decision.TargetMember, fmt.Sprintf("recovery job %s is running", job.Name), true, "RecoveryJobRunning")
	if s.cluster.Status.Recovery != nil {
		s.cluster.Status.Recovery.RetryCount = attempt
	}
	return true, ctrl.Result{RequeueAfter: requeueDuration}, nil
}

func (r *EtcdClusterReconciler) deleteRecoveryTarget(ctx context.Context, s *reconcileState, decision recoveryDecision) error {
	if decision.TargetPod == "" || decision.TargetPVC == "" {
		return fmt.Errorf("recovery target is incomplete")
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: decision.TargetPVC, Namespace: s.cluster.Namespace}}
	if err := r.Delete(ctx, pvc); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete recovery pvc %s/%s: %w", pvc.Namespace, pvc.Name, err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: decision.TargetPod, Namespace: s.cluster.Namespace}}
	if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete recovery pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

func (r *EtcdClusterReconciler) reconcileSingleMemberRecovery(ctx context.Context, s *reconcileState) (bool, ctrl.Result, error) {
	decision := r.evaluateRecoveryPreflight(s)
	if s.cluster.Spec.Recovery == nil || !s.cluster.Spec.Recovery.Enabled {
		return false, ctrl.Result{}, nil
	}
	if !decision.HasUnhealthy {
		if s.cluster.Status.Recovery != nil && s.cluster.Status.Recovery.LastResult == ecv1alpha1.EtcdRecoveryResultRunning {
			r.setRecoveryStatus(s, ecv1alpha1.EtcdRecoveryResultSucceeded, s.cluster.Status.Recovery.LastRecoveredMember, "previous recovery target is healthy", false, "RecoverySucceeded")
			return true, ctrl.Result{}, nil
		}
		return false, ctrl.Result{}, nil
	}
	if !decision.Allowed {
		r.setRecoveryStatus(s, decision.Result, decision.TargetMember, decision.Message, false, decision.Reason)
		return true, ctrl.Result{RequeueAfter: requeueDuration}, nil
	}
	if handled, res, err := r.observeRecoveryJob(ctx, s, decision); handled || err != nil {
		return handled, res, err
	}

	// Re-run the same preflight immediately before creating the Job and deleting
	// the target. This is the controller-side equivalent of a Job-internal
	// preflight and prevents acting on stale state inside this reconcile.
	confirm := r.evaluateRecoveryPreflight(s)
	if !confirm.Allowed || confirm.TargetMember != decision.TargetMember || confirm.TargetPod != decision.TargetPod || confirm.TargetPVC != decision.TargetPVC {
		r.setRecoveryStatus(s, ecv1alpha1.EtcdRecoveryResultBlocked, decision.TargetMember, "recovery preflight changed before execution", false, "RecoveryPreflightChanged")
		return true, ctrl.Result{RequeueAfter: requeueDuration}, nil
	}

	attempt := int32(1)
	if latest := activeRecoveryJobForTarget(s.jobs, decision.TargetMember, ""); latest != nil {
		attempt = recoveryJobAttempt(*latest) + 1
	}
	if attempt > maxRecoveryRetries(s.cluster.Spec.Recovery) {
		r.setRecoveryStatus(s, ecv1alpha1.EtcdRecoveryResultBlocked, decision.TargetMember, fmt.Sprintf("recovery retry count for %s reached maxRetries", decision.TargetMember), false, "MaxRetriesExceeded")
		if s.cluster.Status.Recovery != nil {
			s.cluster.Status.Recovery.RetryCount = attempt - 1
		}
		return true, ctrl.Result{RequeueAfter: requeueDuration}, nil
	}
	job, err := r.createRecoveryJob(ctx, s, decision, attempt)
	if err != nil {
		r.setRecoveryStatus(s, ecv1alpha1.EtcdRecoveryResultFailed, decision.TargetMember, err.Error(), false, "RecoveryJobCreateFailed")
		return true, ctrl.Result{}, err
	}
	if err := r.deleteRecoveryTarget(ctx, s, decision); err != nil {
		r.setRecoveryStatus(s, ecv1alpha1.EtcdRecoveryResultFailed, decision.TargetMember, err.Error(), false, "RecoveryDeleteFailed")
		if s.cluster.Status.Recovery != nil {
			s.cluster.Status.Recovery.RetryCount = attempt
		}
		return true, ctrl.Result{}, err
	}
	if err := r.markRecoveryJobDestructiveExecuted(ctx, job); err != nil {
		r.setRecoveryStatus(s, ecv1alpha1.EtcdRecoveryResultFailed, decision.TargetMember, err.Error(), false, "RecoveryJobPatchFailed")
		if s.cluster.Status.Recovery != nil {
			s.cluster.Status.Recovery.RetryCount = attempt
		}
		return true, ctrl.Result{}, err
	}
	r.setRecoveryStatus(s, ecv1alpha1.EtcdRecoveryResultRunning, decision.TargetMember, fmt.Sprintf("created recovery job %s and deleted Pod %s/PVC %s", job.Name, decision.TargetPod, decision.TargetPVC), true, "RecoveryJobRunning")
	if s.cluster.Status.Recovery != nil {
		s.cluster.Status.Recovery.RetryCount = attempt
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(s.cluster, corev1.EventTypeWarning, "SingleMemberRecoveryStarted", "Created Job %s and deleted Pod %s/PVC %s for recovery", job.Name, decision.TargetPod, decision.TargetPVC)
	}
	return true, ctrl.Result{RequeueAfter: requeueDuration}, nil
}

// reconcileClusterState compares the desired cluster size with the observed
// etcd member list and StatefulSet replica count. It performs scaling actions
// and handles learner promotion when needed. A ctrl.Result with a requeue
// instructs the controller to retry after adjustments.
func (r *EtcdClusterReconciler) reconcileClusterState(ctx context.Context, s *reconcileState) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	memberCnt := 0
	if s.memberListResp != nil {
		memberCnt = len(s.memberListResp.Members)
	}
	targetReplica := *s.sts.Spec.Replicas
	var err error

	// The number of replicas in the StatefulSet doesn't match the number of etcd members in the cluster.
	if int(targetReplica) != memberCnt {
		logger.Info("The expected number of replicas doesn't match the number of etcd members in the cluster", "targetReplica", targetReplica, "memberCnt", memberCnt)
		if int(targetReplica) < memberCnt {
			logger.Info("An etcd member was added into the cluster, but the StatefulSet hasn't scaled out yet")
			newReplicaCount := targetReplica + 1
			logger.Info("Increasing StatefulSet replicas to match the etcd cluster member count", "oldReplicaCount", targetReplica, "newReplicaCount", newReplicaCount)
			if _, err := r.reconcileStatefulSet(ctx, logger, s.cluster, newReplicaCount); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			logger.Info("An etcd member was removed from the cluster, but the StatefulSet hasn't scaled in yet")
			newReplicaCount := targetReplica - 1
			logger.Info("Decreasing StatefulSet replicas to remove the unneeded Pod.", "oldReplicaCount", targetReplica, "newReplicaCount", newReplicaCount)
			if _, err := r.reconcileStatefulSet(ctx, logger, s.cluster, newReplicaCount); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: requeueDuration}, nil
	}

	var (
		learnerStatus *clientv3.StatusResponse
		learner       uint64
		leaderStatus  *clientv3.StatusResponse
	)

	if memberCnt > 0 {
		// Find the leader status
		_, leaderStatus = etcdutils.FindLeaderStatus(s.memberHealth, logger)
		if leaderStatus == nil {
			// If the leader is not available, wait for the leader to be elected
			return ctrl.Result{}, fmt.Errorf("couldn't find leader, memberCnt: %d", memberCnt)
		}

		learner, learnerStatus = etcdutils.FindLearnerStatus(s.memberHealth, logger)
		if learner > 0 {
			// There is at least one learner. Try to promote it if it's ready; otherwise requeue and wait.
			logger.Info("Learner found", "learnedID", learner, "learnerStatus", learnerStatus)
			if etcdutils.IsLearnerReady(leaderStatus, learnerStatus) {
				logger.Info("Learner is ready to be promoted to voting member", "learnerID", learner)
				logger.Info("Promoting the learner member", "learnerID", learner)
				eps := clientEndpointsFromStatefulsets(s.sts, s.tlsConfig)
				eps = eps[:(len(eps) - 1)]
				if err := etcdutils.PromoteLearner(eps, learner, s.tlsConfig); err != nil {
					// The member is not promoted yet, so we error out and requeue via the caller.
					return ctrl.Result{}, err
				}
			} else {
				// Learner is not yet ready. We can't add another learner or proceed further until this one is promoted.
				logger.Info("The learner member isn't ready to be promoted yet", "learnerID", learner)
				return ctrl.Result{RequeueAfter: requeueDuration}, nil
			}
		}
	}

	if handled, res, err := r.reconcileSingleMemberRecovery(ctx, s); handled || err != nil {
		return res, err
	}

	if targetReplica == int32(s.cluster.Spec.Size) {
		if !r.isVersionUpgradeAllowed(ctx, s) {
			return ctrl.Result{}, nil
		}
		updated, err := r.reconcileStatefulSet(ctx, logger, s.cluster, targetReplica)
		if err != nil {
			return ctrl.Result{}, err
		}
		s.sts = updated
		logger.Info("EtcdCluster is already up-to-date")
		return ctrl.Result{}, nil
	}

	eps := clientEndpointsFromStatefulsets(s.sts, s.tlsConfig)

	// If there are no learners left, we can proceed to scale the cluster towards the desired size.
	// When there are no members to add, the controller will requeue above and this block won't execute.
	if targetReplica < int32(s.cluster.Spec.Size) {
		// scale out
		_, peerURL := peerEndpointForOrdinalIndex(s.cluster, int(targetReplica))
		targetReplica++
		logger.Info("[Scale out] adding a new learner member to etcd cluster", "peerURLs", peerURL)
		if _, err := etcdutils.AddMember(eps, []string{peerURL}, true, s.tlsConfig); err != nil {
			return ctrl.Result{}, err
		}

		logger.Info("Learner member added successfully", "peerURLs", peerURL)

		if s.sts, err = r.reconcileStatefulSet(ctx, logger, s.cluster, targetReplica); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: requeueDuration}, nil
	}

	if targetReplica > int32(s.cluster.Spec.Size) {
		// scale in
		targetReplica--
		logger = logger.WithValues("targetReplica", targetReplica, "expectedSize", s.cluster.Spec.Size)

		memberID := s.memberHealth[memberCnt-1].Status.Header.MemberId

		logger.Info("[Scale in] removing one member", "memberID", memberID)
		eps = eps[:targetReplica]
		if err := etcdutils.RemoveMember(eps, memberID, s.tlsConfig); err != nil {
			return ctrl.Result{}, err
		}

		if s.sts, err = r.reconcileStatefulSet(ctx, logger, s.cluster, targetReplica); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: requeueDuration}, nil
	}

	// Ensure every etcd member reports itself healthy before declaring success.
	allMembersHealthy, err := areAllMembersHealthy(s.sts, logger, s.tlsConfig)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !allMembersHealthy {
		// Requeue until the StatefulSet settles and all members are healthy.
		return ctrl.Result{RequeueAfter: requeueDuration}, nil
	}

	logger.Info("EtcdCluster reconciled successfully")
	return ctrl.Result{}, nil
}

func setCondition(conditions *[]metav1.Condition, cond metav1.Condition, generation int64) {
	cond.ObservedGeneration = generation
	for i := range *conditions {
		if (*conditions)[i].Type == cond.Type {
			if (*conditions)[i].Status != cond.Status || (*conditions)[i].Reason != cond.Reason || (*conditions)[i].Message != cond.Message {
				cond.LastTransitionTime = metav1.Now()
			} else {
				cond.LastTransitionTime = (*conditions)[i].LastTransitionTime
			}
			(*conditions)[i] = cond
			return
		}
	}
	cond.LastTransitionTime = metav1.Now()
	*conditions = append(*conditions, cond)
}

func removeCondition(conditions *[]metav1.Condition, conditionType ecv1alpha1.EtcdClusterConditionType) {
	for i := range *conditions {
		if (*conditions)[i].Type == string(conditionType) {
			*conditions = append((*conditions)[:i], (*conditions)[i+1:]...)
			return
		}
	}
}

func setDataStoreCondition(s *reconcileState, cond metav1.Condition) {
	before := s.cluster.Status.DeepCopy()
	setCondition(&s.cluster.Status.Conditions, cond, s.cluster.Generation)
	s.dataStoreConditionSet = true
	s.dataStoreConditionChanged = s.dataStoreConditionChanged || !reflect.DeepEqual(*before, s.cluster.Status)
}

func removeDataStoreCondition(s *reconcileState) {
	before := s.cluster.Status.DeepCopy()
	removeCondition(&s.cluster.Status.Conditions, ecv1alpha1.DataStoreReady)
	changed := !reflect.DeepEqual(*before, s.cluster.Status)
	s.dataStoreConditionSet = s.dataStoreConditionSet || changed
	s.dataStoreConditionChanged = s.dataStoreConditionChanged || changed
}

func buildClusterStatus(ec *ecv1alpha1.EtcdCluster, sts *appsv1.StatefulSet, pods []corev1.Pod, memberList *clientv3.MemberListResponse, health []etcdutils.EpHealth) ecv1alpha1.EtcdClusterStatus {
	status := ec.Status.DeepCopy()
	removeCondition(&status.Conditions, ecv1alpha1.DataStoreReady)
	status.ObservedGeneration = ec.Generation
	if sts != nil {
		status.ReadyReplicas = sts.Status.ReadyReplicas
	}

	healthByID := map[uint64]etcdutils.EpHealth{}
	healthyVoting := 0
	leaderID := uint64(0)
	for _, h := range health {
		if h.Status == nil {
			continue
		}
		id := h.Status.Header.MemberId
		healthByID[id] = h
		if h.Status.Leader == id {
			leaderID = id
		}
		if h.Health && !h.Status.IsLearner {
			healthyVoting++
		}
	}
	if leaderID > 0 {
		status.LeaderID = strconv.FormatUint(leaderID, 16)
	}

	nodeByPod := map[string]string{}
	for _, pod := range pods {
		nodeByPod[pod.Name] = pod.Spec.NodeName
	}

	members := []ecv1alpha1.EtcdMemberStatus{}
	if memberList != nil {
		status.MemberCount = int32(len(memberList.Members))
		for _, m := range memberList.Members {
			ms := ecv1alpha1.EtcdMemberStatus{Name: m.Name, ID: strconv.FormatUint(m.ID, 16), Learner: m.IsLearner, NodeName: nodeByPod[m.Name]}
			if h, ok := healthByID[m.ID]; ok {
				ms.Healthy = h.Health
				ms.Leader = h.Status != nil && h.Status.Leader == m.ID
				ms.Learner = h.Status != nil && h.Status.IsLearner
			}
			members = append(members, ms)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	status.Members = members

	created := sts != nil
	ready := created && int(status.ReadyReplicas) == ec.Spec.Size && int(status.MemberCount) == ec.Spec.Size && len(health) == ec.Spec.Size
	for _, h := range health {
		ready = ready && h.Health && h.Status != nil && !h.Status.IsLearner
	}
	quorum := healthyVoting >= ec.Spec.Size/2+1
	status.Phase = ecv1alpha1.EtcdClusterPhasePending
	if ready && quorum {
		status.Phase = ecv1alpha1.EtcdClusterPhaseReady
	} else if created && status.MemberCount > 0 {
		status.Phase = ecv1alpha1.EtcdClusterPhaseDegraded
	}

	setCondition(&status.Conditions, metav1.Condition{Type: string(ecv1alpha1.EtcdClusterCreated), Status: boolConditionStatus(created), Reason: reasonForBool(created, "ResourcesCreated", "ResourcesPending")}, ec.Generation)
	setCondition(&status.Conditions, metav1.Condition{Type: string(ecv1alpha1.EtcdClusterReady), Status: boolConditionStatus(ready), Reason: reasonForBool(ready, "ClusterReady", "ClusterNotReady")}, ec.Generation)
	setCondition(&status.Conditions, metav1.Condition{Type: string(ecv1alpha1.QuorumAvailable), Status: boolConditionStatus(quorum), Reason: reasonForBool(quorum, "QuorumAvailable", "QuorumUnavailable")}, ec.Generation)
	setCondition(&status.Conditions, metav1.Condition{Type: string(ecv1alpha1.SingleMemberRecoveryActive), Status: metav1.ConditionFalse, Reason: "NoRecoveryActive"}, ec.Generation)
	if ready && status.Recovery != nil && status.Recovery.LastResult == ecv1alpha1.EtcdRecoveryResultRunning {
		now := metav1.Now()
		status.Recovery.LastResult = ecv1alpha1.EtcdRecoveryResultSucceeded
		status.Recovery.LastTransitionTime = &now
		status.Recovery.Message = "previous recovery target is healthy"
		setCondition(&status.Conditions, metav1.Condition{Type: string(ecv1alpha1.SingleMemberRecoveryActive), Status: metav1.ConditionFalse, Reason: "RecoverySucceeded", Message: "previous recovery target is healthy"}, ec.Generation)
	}
	return *status
}

func boolConditionStatus(v bool) metav1.ConditionStatus {
	if v {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func reasonForBool(v bool, trueReason, falseReason string) string {
	if v {
		return trueReason
	}
	return falseReason
}

func (r *EtcdClusterReconciler) updateStatus(ctx context.Context, s *reconcileState) error {
	if s == nil || s.cluster == nil {
		return nil
	}
	newStatus := buildClusterStatus(s.cluster, s.sts, s.pods, s.memberListResp, s.memberHealth)
	// Preserve any condition set directly by safety guards in this reconcile.
	for _, cond := range s.cluster.Status.Conditions {
		if cond.Type == string(ecv1alpha1.EtcdClusterReady) && s.readyGuardConditionSet {
			setCondition(&newStatus.Conditions, cond, s.cluster.Generation)
			newStatus.Phase = ecv1alpha1.EtcdClusterPhaseDegraded
		}
		if cond.Type == string(ecv1alpha1.SingleMemberRecoveryActive) && s.recoveryConditionSet {
			setCondition(&newStatus.Conditions, cond, s.cluster.Generation)
			if cond.Status == metav1.ConditionTrue {
				newStatus.Phase = ecv1alpha1.EtcdClusterPhaseRecovering
			}
		}
		if cond.Type == string(ecv1alpha1.DataStoreReady) && s.dataStoreConditionSet {
			setCondition(&newStatus.Conditions, cond, s.cluster.Generation)
		}
	}
	if reflect.DeepEqual(s.cluster.Status, newStatus) && !s.recoveryConditionSet && !s.dataStoreConditionChanged && !s.readyGuardConditionSet {
		return nil
	}
	patched := s.cluster.DeepCopy()
	patched.Status = newStatus
	return r.Status().Update(ctx, patched)
}

func dataStoreName(ec *ecv1alpha1.EtcdCluster) string {
	if ec.Annotations == nil {
		return ""
	}
	return strings.TrimSpace(ec.Annotations[dataStoreAnnotation])
}

func isEtcdClusterReadyForDataStore(s *reconcileState) bool {
	if s == nil || s.cluster == nil || s.sts == nil || s.memberListResp == nil {
		return false
	}
	if int(s.sts.Status.ReadyReplicas) != s.cluster.Spec.Size || len(s.memberListResp.Members) != s.cluster.Spec.Size || len(s.memberHealth) != s.cluster.Spec.Size {
		return false
	}
	for _, h := range s.memberHealth {
		if !h.Health || h.Status == nil || h.Status.IsLearner {
			return false
		}
	}
	return true
}

func dataStoreEndpoint(ec *ecv1alpha1.EtcdCluster) string {
	return fmt.Sprintf("%s.%s.svc:2379", clientServiceNameForEtcdCluster(ec), ec.Namespace)
}

func dataStoreSecretReference(secretName, namespace, keyPath string) map[string]any {
	return map[string]any{
		"secretReference": map[string]any{
			"name":      secretName,
			"namespace": namespace,
			"keyPath":   keyPath,
		},
	}
}

func desiredDataStoreSpec(ec *ecv1alpha1.EtcdCluster, caSecretName, caSecretNamespace string) map[string]any {
	clientSecret := getClientCertName(ec.Name)
	return map[string]any{
		"driver":    "etcd",
		"endpoints": []any{dataStoreEndpoint(ec)},
		"basicAuth": nil,
		"tlsConfig": map[string]any{
			"certificateAuthority": map[string]any{
				"certificate": dataStoreSecretReference(caSecretName, caSecretNamespace, corev1.TLSCertKey),
				"privateKey":  dataStoreSecretReference(caSecretName, caSecretNamespace, corev1.TLSPrivateKeyKey),
			},
			"clientCertificate": map[string]any{
				"certificate": dataStoreSecretReference(clientSecret, ec.Namespace, corev1.TLSCertKey),
				"privateKey":  dataStoreSecretReference(clientSecret, ec.Namespace, corev1.TLSPrivateKeyKey),
			},
		},
	}
}

func desiredDataStore(ec *ecv1alpha1.EtcdCluster, name, caSecretName, caSecretNamespace string) *unstructured.Unstructured {
	ds := &unstructured.Unstructured{}
	ds.SetGroupVersionKind(dataStoreGVK)
	ds.SetName(name)
	ds.SetLabels(map[string]string{
		"app":        ec.Name,
		"controller": ec.Name,
	})
	ds.Object["spec"] = desiredDataStoreSpec(ec, caSecretName, caSecretNamespace)
	return ds
}

func hasTLSKeyPair(secret *corev1.Secret) bool {
	if secret == nil || secret.Data == nil {
		return false
	}
	return len(secret.Data[corev1.TLSCertKey]) > 0 && len(secret.Data[corev1.TLSPrivateKeyKey]) > 0
}

func (r *EtcdClusterReconciler) resolveDataStoreCASecret(ctx context.Context, ec *ecv1alpha1.EtcdCluster) (string, string, error) {
	if ec.Spec.TLS == nil || ec.Spec.TLS.Provider != "cert-manager" || ec.Spec.TLS.ProviderCfg.CertManagerCfg == nil {
		return "", "", fmt.Errorf("DataStore requires TLS provider cert-manager with certManagerCfg")
	}
	cmConfig := ec.Spec.TLS.ProviderCfg.CertManagerCfg
	if cmConfig.IssuerKind != "Issuer" {
		return "", "", fmt.Errorf("DataStore automatic CA discovery supports cert-manager Issuer only, got %q", cmConfig.IssuerKind)
	}
	issuer := &certv1.Issuer{}
	if err := r.Get(ctx, client.ObjectKey{Name: cmConfig.IssuerName, Namespace: ec.Namespace}, issuer); err != nil {
		return "", "", fmt.Errorf("get cert-manager Issuer %s/%s: %w", ec.Namespace, cmConfig.IssuerName, err)
	}
	if issuer.Spec.CA == nil || issuer.Spec.CA.SecretName == "" {
		return "", "", fmt.Errorf("Issuer %s/%s is not a CA issuer with spec.ca.secretName", issuer.Namespace, issuer.Name)
	}
	caSecret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: issuer.Spec.CA.SecretName, Namespace: ec.Namespace}, caSecret); err != nil {
		return "", "", fmt.Errorf("get CA Secret %s/%s referenced by Issuer %s: %w", ec.Namespace, issuer.Spec.CA.SecretName, issuer.Name, err)
	}
	if !hasTLSKeyPair(caSecret) {
		return "", "", fmt.Errorf("CA Secret %s/%s must contain %s and %s", caSecret.Namespace, caSecret.Name, corev1.TLSCertKey, corev1.TLSPrivateKeyKey)
	}
	clientSecret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: getClientCertName(ec.Name), Namespace: ec.Namespace}, clientSecret); err != nil {
		return "", "", fmt.Errorf("get client certificate Secret %s/%s: %w", ec.Namespace, getClientCertName(ec.Name), err)
	}
	if !hasTLSKeyPair(clientSecret) {
		return "", "", fmt.Errorf("client certificate Secret %s/%s must contain %s and %s", clientSecret.Namespace, clientSecret.Name, corev1.TLSCertKey, corev1.TLSPrivateKeyKey)
	}
	return caSecret.Name, caSecret.Namespace, nil
}

func (r *EtcdClusterReconciler) reconcileDataStore(ctx context.Context, s *reconcileState) error {
	name := dataStoreName(s.cluster)
	if name == "" {
		removeDataStoreCondition(s)
		return nil
	}
	if !isEtcdClusterReadyForDataStore(s) {
		setDataStoreCondition(s, metav1.Condition{
			Type:    string(ecv1alpha1.DataStoreReady),
			Status:  metav1.ConditionFalse,
			Reason:  "EtcdClusterNotReady",
			Message: "DataStore is not reconciled until EtcdCluster is Ready",
		})
		return nil
	}

	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(dataStoreGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: name}, current); err != nil {
		if apimeta.IsNoMatchError(err) {
			setDataStoreCondition(s, metav1.Condition{Type: string(ecv1alpha1.DataStoreReady), Status: metav1.ConditionFalse, Reason: "DataStoreCRDNotFound", Message: "Kamaji DataStore CRD is not installed"})
			return nil
		}
		if !errors.IsNotFound(err) {
			return err
		}

		caSecretName, caSecretNamespace, err := r.resolveDataStoreCASecret(ctx, s.cluster)
		if err != nil {
			setDataStoreCondition(s, metav1.Condition{Type: string(ecv1alpha1.DataStoreReady), Status: metav1.ConditionFalse, Reason: "DataStoreCASecretUnavailable", Message: err.Error()})
			return nil
		}
		desired := desiredDataStore(s.cluster, name, caSecretName, caSecretNamespace)
		if err := r.Create(ctx, desired); err != nil {
			if apimeta.IsNoMatchError(err) {
				setDataStoreCondition(s, metav1.Condition{Type: string(ecv1alpha1.DataStoreReady), Status: metav1.ConditionFalse, Reason: "DataStoreCRDNotFound", Message: "Kamaji DataStore CRD is not installed"})
				return nil
			}
			return err
		}
		setDataStoreCondition(s, metav1.Condition{Type: string(ecv1alpha1.DataStoreReady), Status: metav1.ConditionTrue, Reason: "DataStoreReady", Message: fmt.Sprintf("DataStore %s created", name)})
		if r.Recorder != nil {
			r.Recorder.Eventf(s.cluster, corev1.EventTypeNormal, "DataStoreReady", "Created DataStore %s", name)
		}
		return nil
	}

	caSecretName, caSecretNamespace, err := r.resolveDataStoreCASecret(ctx, s.cluster)
	if err != nil {
		setDataStoreCondition(s, metav1.Condition{Type: string(ecv1alpha1.DataStoreReady), Status: metav1.ConditionFalse, Reason: "DataStoreCASecretUnavailable", Message: err.Error()})
		return nil
	}
	desired := desiredDataStore(s.cluster, name, caSecretName, caSecretNamespace)
	changed := false
	if !reflect.DeepEqual(current.Object["spec"], desired.Object["spec"]) {
		current.Object["spec"] = desired.Object["spec"]
		changed = true
	}
	labels := current.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for key, value := range desired.GetLabels() {
		if labels[key] != value {
			labels[key] = value
			changed = true
		}
	}
	current.SetLabels(labels)
	if changed {
		if err := r.Update(ctx, current); err != nil {
			if apimeta.IsNoMatchError(err) {
				setDataStoreCondition(s, metav1.Condition{Type: string(ecv1alpha1.DataStoreReady), Status: metav1.ConditionFalse, Reason: "DataStoreCRDNotFound", Message: "Kamaji DataStore CRD is not installed"})
				return nil
			}
			return err
		}
	}
	setDataStoreCondition(s, metav1.Condition{Type: string(ecv1alpha1.DataStoreReady), Status: metav1.ConditionTrue, Reason: "DataStoreReady", Message: fmt.Sprintf("DataStore %s is reconciled", name)})
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *EtcdClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("etcdcluster-controller")
	return ctrl.NewControllerManagedBy(mgr).
		For(&ecv1alpha1.EtcdCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&batchv1.Job{}).
		Owns(&certv1.Certificate{}).
		Complete(r)
}
