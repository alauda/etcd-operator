package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ecv1alpha1 "go.etcd.io/etcd-operator/api/v1alpha1"
	"go.etcd.io/etcd-operator/internal/etcdutils"
	"go.etcd.io/etcd-operator/pkg/certificate"
	certInterface "go.etcd.io/etcd-operator/pkg/certificate/interfaces"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	etcdDataDir                  = "/var/lib/etcd"
	volumeName                   = "etcd-data"
	defaultEtcdPriorityClassName = "system-cluster-critical"
	etcdClientServiceNameSuffix  = "-client"
	etcdProbeContainerName       = "etcd-probe"
	etcdProbePortName            = "probe"
	etcdProbePort                = 9980
	etcdProbeCertMountPath       = "/etc/etcd/certs/client"
	resetMemberContainerName     = "reset-member"
)

type StatefulSetOptions struct {
	ProbeImage string
}

func firstStatefulSetOptions(options []StatefulSetOptions) StatefulSetOptions {
	if len(options) == 0 {
		return StatefulSetOptions{}
	}
	return options[0]
}

type etcdClusterState string

const (
	etcdClusterStateNew      etcdClusterState = "new"
	etcdClusterStateExisting etcdClusterState = "existing"
)

func reconcileStatefulSet(ctx context.Context, logger logr.Logger, ec *ecv1alpha1.EtcdCluster, c client.Client, replicas int32, scheme *runtime.Scheme, options ...StatefulSetOptions) (*appsv1.StatefulSet, error) {

	// prepare/update configmap for StatefulSet
	err := applyEtcdClusterState(ctx, ec, int(replicas), c, scheme, logger)
	if err != nil {
		return nil, err
	}

	// Add server and peer certificate
	err = applyEtcdMemberCerts(ctx, ec, c, logger)
	if err != nil {
		return nil, err
	}

	// Create Update StatefulSet
	err = createOrPatchStatefulSet(ctx, logger, ec, c, replicas, scheme, options...)
	if err != nil {
		return nil, err
	}

	// Wait for statefulset to be ready
	err = waitForStatefulSetReady(ctx, logger, c, ec.Name, ec.Namespace)
	if err != nil {
		return nil, err
	}

	// Return latest Stateful set. (This is to ensure that we return the latest statefulset for next operation to act on)
	return getStatefulSet(ctx, c, ec.Name, ec.Namespace)
}

func defaultArgs(name string) []string {
	return []string{
		"--name=$(POD_NAME)",

		"--listen-peer-urls=http://0.0.0.0:2380",   // TODO: only listen on 127.0.0.1 and host IP
		"--listen-client-urls=http://0.0.0.0:2379", // TODO: only listen on 127.0.0.1 and host IP
		fmt.Sprintf("--initial-advertise-peer-urls=http://$(POD_NAME).%s.$(POD_NAMESPACE).svc.cluster.local:2380", name),
		fmt.Sprintf("--advertise-client-urls=http://$(POD_NAME).%s.$(POD_NAMESPACE).svc.cluster.local:2379", name),
	}
}

func defaultTLSArgs(name string) []string {
	return []string{
		"--name=$(POD_NAME)",

		"--listen-peer-urls=https://0.0.0.0:2380",   // TODO: only listen on 127.0.0.1 and host IP
		"--listen-client-urls=https://0.0.0.0:2379", // TODO: only listen on 127.0.0.1 and host IP
		fmt.Sprintf("--initial-advertise-peer-urls=https://$(POD_NAME).%s.$(POD_NAMESPACE).svc.cluster.local:2380", name),
		fmt.Sprintf("--advertise-client-urls=https://$(POD_NAME).%s.$(POD_NAMESPACE).svc.cluster.local:2379", name),

		"--client-cert-auth",
		"--trusted-ca-file=/etc/etcd/certs/server/ca.crt",
		"--cert-file=/etc/etcd/certs/server/tls.crt",
		"--key-file=/etc/etcd/certs/server/tls.key",

		"--peer-client-cert-auth",
		"--peer-trusted-ca-file=/etc/etcd/certs/peer/ca.crt",
		"--peer-cert-file=/etc/etcd/certs/peer/tls.crt",
		"--peer-key-file=/etc/etcd/certs/peer/tls.key",
	}
}

func RemoveStringFromSlice(s []string, str string) []string {
	for i := range s {
		defaultArg := getArgName(s[i])
		if defaultArg == str {
			s = slices.Delete(s, i, i+1)
			break
		}
	}
	return s
}

func getArgName(s string) string {
	idx := strings.Index(s, "=")

	if idx != -1 {
		return s[:idx]
	}

	idx = strings.Index(s, " ")
	if idx != -1 {
		return s[:idx]
	}

	// Assume arg is bool switch if idx is still -1
	return strings.TrimSpace(s)
}

func createArgs(name string, etcdOptions []string, tls bool) []string {
	defaultArgs := defaultArgs(name)
	if tls {
		defaultArgs = defaultTLSArgs(name)
	}
	if len(etcdOptions) > 0 {
		var argName string
		// Remove default arguments if conflicts with user supplied
		for i := range etcdOptions {
			argName = getArgName(etcdOptions[i])
			defaultArgs = RemoveStringFromSlice(defaultArgs, argName)
		}
	}
	defaultArgs = append(defaultArgs, etcdOptions...)
	return defaultArgs
}

func labelsForEtcdCluster(ec *ecv1alpha1.EtcdCluster) map[string]string {
	return map[string]string{
		"app":        ec.Name,
		"controller": ec.Name,
	}
}

func clientServiceNameForEtcdCluster(ec *ecv1alpha1.EtcdCluster) string {
	return ec.Name + etcdClientServiceNameSuffix
}

func applyPodTemplateSpec(podTemplate *ecv1alpha1.PodTemplate, podSpec *corev1.PodSpec) {
	if podTemplate == nil || podTemplate.Spec == nil {
		return
	}

	templateSpec := podTemplate.Spec
	if len(templateSpec.NodeSelector) > 0 {
		podSpec.NodeSelector = templateSpec.NodeSelector
	}
	if len(templateSpec.Tolerations) > 0 {
		podSpec.Tolerations = templateSpec.Tolerations
	}
	if templateSpec.Affinity != nil {
		podSpec.Affinity = templateSpec.Affinity
	}
	if len(templateSpec.TopologySpreadConstraints) > 0 {
		podSpec.TopologySpreadConstraints = templateSpec.TopologySpreadConstraints
	}
	if templateSpec.PriorityClassName != "" {
		podSpec.PriorityClassName = templateSpec.PriorityClassName
	}
}

func etcdProbe(path string, periodSeconds, failureThreshold, timeoutSeconds int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: path,
				Port: intstr.FromInt32(etcdProbePort),
			},
		},
		PeriodSeconds:    periodSeconds,
		FailureThreshold: failureThreshold,
		TimeoutSeconds:   timeoutSeconds,
	}
}

func probeEndpointForEtcdCluster(ec *ecv1alpha1.EtcdCluster) string {
	scheme := "http"
	if ec.Spec.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://$(POD_NAME).$(ETCD_SERVICE_NAME).$(POD_NAMESPACE).svc.cluster.local:2379", scheme)
}

func probeContainerForEtcdCluster(ec *ecv1alpha1.EtcdCluster, image string) corev1.Container {
	args := []string{
		"--listen-address=:9980",
		fmt.Sprintf("--endpoint=%s", probeEndpointForEtcdCluster(ec)),
		"--timeout=30s",
	}
	volumeMounts := []corev1.VolumeMount{}
	if ec.Spec.TLS != nil {
		args = append(args,
			fmt.Sprintf("--cacert=%s/ca.crt", etcdProbeCertMountPath),
			fmt.Sprintf("--cert=%s/tls.crt", etcdProbeCertMountPath),
			fmt.Sprintf("--key=%s/tls.key", etcdProbeCertMountPath),
		)
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "client-secret",
			MountPath: etcdProbeCertMountPath,
		})
	}

	return corev1.Container{
		Name:    etcdProbeContainerName,
		Image:   image,
		Command: []string{"/etcd-probe"},
		Args:    args,
		Env: []corev1.EnvVar{
			{
				Name: "POD_NAME",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
				},
			},
			{
				Name: "POD_NAMESPACE",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
				},
			},
			{Name: "ETCD_SERVICE_NAME", Value: ec.Name},
			{Name: "ETCD_REPLICAS", Value: strconv.Itoa(ec.Spec.Size)},
		},
		Ports: []corev1.ContainerPort{{
			Name:          etcdProbePortName,
			ContainerPort: etcdProbePort,
		}},
		VolumeMounts: volumeMounts,
	}
}

func resetMemberInitContainerForEtcdCluster(ec *ecv1alpha1.EtcdCluster) corev1.Container {
	image := fmt.Sprintf("%s:%s", ec.Spec.ImageRegistry, ec.Spec.Version)
	args := []string{`set -eu
if [ -f /var/lib/etcd/member/snap/db ]; then
  echo "member has data; reset-member no-op"
  exit 0
fi

scheme="http"
etcdctl_tls_args=""
if [ -f /etc/etcd/certs/client/ca.crt ]; then
  scheme="https"
  etcdctl_tls_args="--cacert=/etc/etcd/certs/client/ca.crt --cert=/etc/etcd/certs/client/tls.crt --key=/etc/etcd/certs/client/tls.key"
fi
endpoints=""
for i in $(seq 0 $((ETCD_REPLICAS - 1))); do
  member_name="${ETCD_SERVICE_NAME}-${i}"
  if [ "${member_name}" = "${POD_NAME}" ]; then
    continue
  fi
  ep="${scheme}://${member_name}.${ETCD_SERVICE_NAME}.${POD_NAMESPACE}.svc.cluster.local:2379"
  if [ -z "${endpoints}" ]; then
    endpoints="${ep}"
  else
    endpoints="${endpoints},${ep}"
  fi
done
peer_url="${scheme}://${POD_NAME}.${ETCD_SERVICE_NAME}.${POD_NAMESPACE}.svc.cluster.local:2380"

export ETCDCTL_API=3
members="$(etcdctl ${etcdctl_tls_args} --endpoints="${endpoints}" member list -w simple)"
old_id="$(printf '%s\n' "${members}" | awk -F',' -v name="${POD_NAME}" '{gsub(/^[ \t]+|[ \t]+$/, "", $1); gsub(/^[ \t]+|[ \t]+$/, "", $3); if ($3 == name) {print $1; exit}}')"
if [ -n "${old_id}" ]; then
  echo "removing stale member ${POD_NAME} (${old_id})"
  etcdctl ${etcdctl_tls_args} --endpoints="${endpoints}" member remove "${old_id}"
else
  echo "no stale member named ${POD_NAME} found"
fi

echo "adding member ${POD_NAME} with peer URL ${peer_url}"
etcdctl ${etcdctl_tls_args} --endpoints="${endpoints}" member add "${POD_NAME}" --peer-urls="${peer_url}"
`}
	volumeMounts := []corev1.VolumeMount{{Name: volumeName, MountPath: etcdDataDir, SubPathExpr: "$(POD_NAME)"}}
	if ec.Spec.TLS != nil {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "client-secret", MountPath: etcdProbeCertMountPath})
	}
	return corev1.Container{
		Name:         resetMemberContainerName,
		Image:        image,
		Command:      []string{"/bin/sh", "-c"},
		Args:         args,
		VolumeMounts: volumeMounts,
		Env: []corev1.EnvVar{
			{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
			{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			{Name: "ETCD_SERVICE_NAME", Value: ec.Name},
			{Name: "ETCD_REPLICAS", Value: strconv.Itoa(ec.Spec.Size)},
		},
	}
}

func createOrPatchStatefulSet(ctx context.Context, logger logr.Logger, ec *ecv1alpha1.EtcdCluster, c client.Client, replicas int32, scheme *runtime.Scheme, options ...StatefulSetOptions) error {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ec.Name,
			Namespace: ec.Namespace,
		},
	}

	labels := labelsForEtcdCluster(ec)
	stsOptions := firstStatefulSetOptions(options)

	podSpec := corev1.PodSpec{
		PriorityClassName: defaultEtcdPriorityClassName,
		Containers: []corev1.Container{
			{
				Name:    "etcd",
				Command: []string{"/usr/local/bin/etcd"},
				Args:    createArgs(ec.Name, ec.Spec.EtcdOptions, ec.Spec.TLS != nil),
				Image:   fmt.Sprintf("%s:%s", ec.Spec.ImageRegistry, ec.Spec.Version),
				Env: []corev1.EnvVar{
					{
						Name: "POD_NAME",
						ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "metadata.name",
							},
						},
					},
					{
						Name: "POD_NAMESPACE",
						ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "metadata.namespace",
							},
						},
					},
				},
				EnvFrom: []corev1.EnvFromSource{
					{
						ConfigMapRef: &corev1.ConfigMapEnvSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: configMapNameForEtcdCluster(ec),
							},
						},
					},
				},
				Ports: []corev1.ContainerPort{
					{
						Name:          "client",
						ContainerPort: 2379,
					},
					{
						Name:          "peer",
						ContainerPort: 2380,
					},
				},
			},
		},
	}

	probeImage := stsOptions.ProbeImage
	if probeImage != "" {
		podSpec.Containers[0].LivenessProbe = etcdProbe("/healthz", 5, 5, 30)
		podSpec.Containers[0].ReadinessProbe = etcdProbe("/readyz", 5, 15, 30)
		podSpec.Containers[0].StartupProbe = etcdProbe("/readyz", 10, 18, 30)
		podSpec.Containers = append(podSpec.Containers, probeContainerForEtcdCluster(ec, probeImage))
	}

	// mount server and peer certificate secret to each pods of the statefulset via PodSpec
	var certVolume []corev1.Volume
	serverCertName := getServerCertName(ec.Name)
	peerCertName := getPeerCertName(ec.Name)
	if ec.Spec.TLS != nil {
		clientCertName := getClientCertName(ec.Name)
		serverCertVolume := corev1.Volume{
			Name: "server-secret",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: serverCertName},
			},
		}
		peerCertVolume := corev1.Volume{
			Name: "peer-secret",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: peerCertName},
			},
		}
		certVolume = append(certVolume, serverCertVolume, peerCertVolume)
		if probeImage != "" || (ec.Spec.StorageSpec != nil && ec.Spec.Recovery != nil && ec.Spec.Recovery.Enabled) {
			certVolume = append(certVolume, corev1.Volume{
				Name: "client-secret",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: clientCertName},
				},
			})
		}

		certVolumeMount := []corev1.VolumeMount{
			{
				Name:      "server-secret",
				MountPath: "/etc/etcd/certs/server",
			},
			{
				Name:      "peer-secret",
				MountPath: "/etc/etcd/certs/peer",
			},
		}

		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, certVolumeMount...)
	}
	if len(certVolume) != 0 {
		podSpec.Volumes = certVolume
	}

	applyPodTemplateSpec(ec.Spec.PodTemplate, &podSpec)
	if ec.Spec.StorageSpec != nil && ec.Spec.Recovery != nil && ec.Spec.Recovery.Enabled {
		podSpec.InitContainers = append(podSpec.InitContainers, resetMemberInitContainerForEtcdCluster(ec))
	}

	// Prepare pod template metadata
	podTemplateMetadata := metav1.ObjectMeta{
		Labels:      make(map[string]string),
		Annotations: make(map[string]string),
	}

	// Apply custom metadata from PodTemplate if provided
	if ec.Spec.PodTemplate != nil && ec.Spec.PodTemplate.Metadata != nil {
		// Apply custom labels
		if len(ec.Spec.PodTemplate.Metadata.Labels) > 0 {
			maps.Copy(podTemplateMetadata.Labels, ec.Spec.PodTemplate.Metadata.Labels)
		}

		// Apply annotations
		if len(ec.Spec.PodTemplate.Metadata.Annotations) > 0 {
			podTemplateMetadata.Annotations = ec.Spec.PodTemplate.Metadata.Annotations
		}
	}

	// Apply default labels
	maps.Copy(podTemplateMetadata.Labels, labels)

	stsSpec := appsv1.StatefulSetSpec{
		Replicas:            &replicas,
		ServiceName:         ec.Name,
		PodManagementPolicy: appsv1.ParallelPodManagement,
		Selector: &metav1.LabelSelector{
			MatchLabels: labels,
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: podTemplateMetadata,
			Spec:       podSpec,
		},
	}

	if ec.Spec.StorageSpec != nil {
		stsSpec.Template.Spec.Containers[0].VolumeMounts = append(stsSpec.Template.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name:        volumeName,
			MountPath:   etcdDataDir,
			SubPathExpr: "$(POD_NAME)",
		})

		// Create a new volume claim template
		if ec.Spec.StorageSpec.VolumeSizeRequest.Cmp(resource.MustParse("1Mi")) < 0 {
			return fmt.Errorf("VolumeSizeRequest must be at least 1Mi")
		}

		if ec.Spec.StorageSpec.VolumeSizeLimit.IsZero() {
			logger.Info("VolumeSizeLimit is not set. Setting it to VolumeSizeRequest")
			ec.Spec.StorageSpec.VolumeSizeLimit = ec.Spec.StorageSpec.VolumeSizeRequest
		}

		pvcResources := corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceStorage: ec.Spec.StorageSpec.VolumeSizeRequest,
			},
			Limits: corev1.ResourceList{
				corev1.ResourceStorage: ec.Spec.StorageSpec.VolumeSizeLimit,
			},
		}

		switch ec.Spec.StorageSpec.AccessModes {
		case corev1.ReadWriteOnce, "":
			stsSpec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: volumeName},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources:   pvcResources,
					},
				},
			}

			if ec.Spec.StorageSpec.StorageClassName != "" {
				stsSpec.VolumeClaimTemplates[0].Spec.StorageClassName = &ec.Spec.StorageSpec.StorageClassName
			}
		case corev1.ReadWriteMany:
			if ec.Spec.StorageSpec.PVCName == "" {
				return fmt.Errorf("PVCName must be set when AccessModes is ReadWriteMany")
			}
			stsSpec.Template.Spec.Volumes = append(stsSpec.Template.Spec.Volumes, corev1.Volume{
				Name: volumeName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: ec.Spec.StorageSpec.PVCName,
					},
				},
			})
		default:
			return fmt.Errorf("AccessMode %s is not supported", ec.Spec.StorageSpec.AccessModes)
		}
	}

	isCreate := true
	if err := c.Get(ctx, types.NamespacedName{Name: ec.Name, Namespace: ec.Namespace}, &appsv1.StatefulSet{}); err != nil {
		if !k8serrors.IsNotFound(err) {
			return err
		}
	} else {
		isCreate = false
	}

	logger.Info("Now creating/updating statefulset", "name", ec.Name, "namespace", ec.Namespace, "replicas", replicas)
	_, err := controllerutil.CreateOrPatch(ctx, c, sts, func() error {
		if isCreate {
			sts.Spec = stsSpec
		} else {
			sts.Spec.Replicas = stsSpec.Replicas
			sts.Spec.Template = stsSpec.Template
		}

		sts.Labels = labels

		// Set owner reference
		if err := controllerutil.SetControllerReference(ec, sts, scheme); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	logger.Info("Stateful set created/updated", "name", ec.Name, "namespace", ec.Namespace, "replicas", replicas)
	return nil
}

func waitForStatefulSetReady(ctx context.Context, logger logr.Logger, r client.Client, name, namespace string) error {
	logger.Info("Now checking the readiness of statefulset", "name", name, "namespace", namespace)

	backoff := wait.Backoff{
		Duration: 3 * time.Second,
		Factor:   2.0,
		Steps:    5,
	}

	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		// Fetch the StatefulSet
		sts, err := getStatefulSet(ctx, r, name, namespace)
		if err != nil {
			return false, err
		}

		// Check if the StatefulSet is ready
		if sts.Status.ReadyReplicas == *sts.Spec.Replicas {
			// StatefulSet is ready
			logger.Info("StatefulSet is ready", "name", name, "namespace", namespace)
			return true, nil
		}

		// Log the current status
		logger.Info("StatefulSet is not ready", "ReadyReplicas", strconv.Itoa(int(sts.Status.ReadyReplicas)), "DesiredReplicas", strconv.Itoa(int(*sts.Spec.Replicas)))
		return false, nil
	})

	if err != nil {
		return fmt.Errorf("StatefulSet %s/%s did not become ready: %w", namespace, name, err)
	}

	return nil
}

func applyMutableServiceFields(service *corev1.Service, labels map[string]string, publishNotReady bool, ports []corev1.ServicePort) {
	service.Labels = labels
	service.Spec.Selector = labels
	service.Spec.PublishNotReadyAddresses = publishNotReady
	service.Spec.Ports = ports
}

func createHeadlessServiceIfNotExist(ctx context.Context, logger logr.Logger, c client.Client, ec *ecv1alpha1.EtcdCluster, scheme *runtime.Scheme) error {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ec.Name,
			Namespace: ec.Namespace,
		},
	}
	labels := labelsForEtcdCluster(ec)

	isCreate := true
	if err := c.Get(ctx, types.NamespacedName{Name: ec.Name, Namespace: ec.Namespace}, &corev1.Service{}); err != nil {
		if !k8serrors.IsNotFound(err) {
			return err
		}
	} else {
		isCreate = false
	}

	logger.Info("Now creating/updating headless service", "name", ec.Name, "namespace", ec.Namespace)
	_, err := controllerutil.CreateOrPatch(ctx, c, service, func() error {
		if isCreate {
			service.Spec.ClusterIP = "None" // Key for headless service
		}
		applyMutableServiceFields(service, labels, true, nil)
		return controllerutil.SetControllerReference(ec, service, scheme)
	})
	if err != nil {
		return fmt.Errorf("failed to create or patch headless service: %w", err)
	}
	logger.Info("Headless service created/updated successfully")
	return nil
}

func createClientServiceIfNotExist(ctx context.Context, logger logr.Logger, c client.Client, ec *ecv1alpha1.EtcdCluster, scheme *runtime.Scheme) error {
	serviceName := clientServiceNameForEtcdCluster(ec)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: ec.Namespace,
		},
	}
	labels := labelsForEtcdCluster(ec)
	ports := []corev1.ServicePort{
		{
			Name:       "client",
			Port:       2379,
			TargetPort: intstr.FromString("client"),
		},
	}

	isCreate := true
	if err := c.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: ec.Namespace}, &corev1.Service{}); err != nil {
		if !k8serrors.IsNotFound(err) {
			return err
		}
	} else {
		isCreate = false
	}

	logger.Info("Now creating/updating client service", "name", serviceName, "namespace", ec.Namespace)
	_, err := controllerutil.CreateOrPatch(ctx, c, service, func() error {
		if isCreate {
			service.Spec.ClusterIP = "None"
		}
		applyMutableServiceFields(service, labels, false, ports)
		return controllerutil.SetControllerReference(ec, service, scheme)
	})
	if err != nil {
		return fmt.Errorf("failed to create or patch client service: %w", err)
	}
	logger.Info("Client service created/updated successfully", "name", serviceName)
	return nil
}

func createOrPatchPodDisruptionBudget(ctx context.Context, logger logr.Logger, c client.Client, ec *ecv1alpha1.EtcdCluster, scheme *runtime.Scheme) error {
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ec.Name,
			Namespace: ec.Namespace,
		},
	}

	labels := labelsForEtcdCluster(ec)
	maxUnavailable := intstr.FromInt(1)
	unhealthyPodEvictionPolicy := policyv1.AlwaysAllow

	logger.Info("Now creating/updating pod disruption budget", "name", ec.Name, "namespace", ec.Namespace)
	_, err := controllerutil.CreateOrPatch(ctx, c, pdb, func() error {
		pdb.Labels = labels
		pdb.Spec.MaxUnavailable = &maxUnavailable
		pdb.Spec.MinAvailable = nil
		pdb.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: labels,
		}
		pdb.Spec.UnhealthyPodEvictionPolicy = &unhealthyPodEvictionPolicy

		if err := controllerutil.SetControllerReference(ec, pdb, scheme); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}

	logger.Info("Pod disruption budget created/updated", "name", ec.Name, "namespace", ec.Namespace)
	return nil
}

func checkStatefulSetControlledByEtcdOperator(ec *ecv1alpha1.EtcdCluster, sts *appsv1.StatefulSet) error {
	if !metav1.IsControlledBy(sts, ec) {
		return fmt.Errorf("StatefulSet %s/%s is not controlled by EtcdCluster %s/%s", sts.Namespace, sts.Name, ec.Namespace, ec.Name)
	}
	return nil
}

func configMapNameForEtcdCluster(ec *ecv1alpha1.EtcdCluster) string {
	return fmt.Sprintf("%s-state", ec.Name)
}

func peerEndpointForOrdinalIndex(ec *ecv1alpha1.EtcdCluster, index int) (string, string) {
	name := fmt.Sprintf("%s-%d", ec.Name, index)
	uriScheme := "http"
	if ec.Spec.TLS != nil {
		uriScheme = "https"
	}
	return name, fmt.Sprintf("%s://%s-%d.%s.%s.svc.cluster.local:2380",
		uriScheme, ec.Name, index, ec.Name, ec.Namespace)
}

func newEtcdClusterState(ec *ecv1alpha1.EtcdCluster, replica int) *corev1.ConfigMap {
	// We always add members one by one, so the state is always
	// "existing" if replica > 1.

	state := etcdClusterStateNew
	if replica > 1 {
		state = etcdClusterStateExisting
	}

	var initialCluster []string
	for i := range replica {
		name, peerURL := peerEndpointForOrdinalIndex(ec, i)
		initialCluster = append(initialCluster, fmt.Sprintf("%s=%s", name, peerURL))
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapNameForEtcdCluster(ec),
			Namespace: ec.Namespace,
		},
		Data: map[string]string{
			"ETCD_INITIAL_CLUSTER_STATE": string(state),
			"ETCD_INITIAL_CLUSTER":       strings.Join(initialCluster, ","),
			"ETCD_DATA_DIR":              etcdDataDir,
		},
	}
}

func applyEtcdClusterState(ctx context.Context, ec *ecv1alpha1.EtcdCluster, replica int, c client.Client, scheme *runtime.Scheme, logger logr.Logger) error {
	cm := newEtcdClusterState(ec, replica)

	// Set owner reference
	if err := controllerutil.SetControllerReference(ec, cm, scheme); err != nil {
		return err
	}

	logger.Info("Now updating configmap", "name", configMapNameForEtcdCluster(ec), "namespace", ec.Namespace)
	err := c.Get(ctx, types.NamespacedName{Name: configMapNameForEtcdCluster(ec), Namespace: ec.Namespace}, &corev1.ConfigMap{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			createErr := c.Create(ctx, cm)
			return createErr
		}
		return err
	}

	updateErr := c.Update(ctx, cm)
	return updateErr
}

func clientEndpointForOrdinalIndex(sts *appsv1.StatefulSet, index int, tlsConfig *tls.Config) string {
	uriScheme := "http"
	if tlsConfig != nil {
		uriScheme = "https"
	}
	return fmt.Sprintf("%s://%s-%d.%s.%s.svc.cluster.local:2379",
		uriScheme,
		sts.Name,
		index,
		sts.Name,
		sts.Namespace,
	)
}

func getStatefulSet(ctx context.Context, c client.Client, name, namespace string) (*appsv1.StatefulSet, error) {
	sts := &appsv1.StatefulSet{}
	err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, sts)
	if err != nil {
		return nil, err
	}
	return sts, nil
}

func clientEndpointsFromStatefulsets(sts *appsv1.StatefulSet, tlsConfig *tls.Config) []string {
	var endpoints []string
	replica := int(*sts.Spec.Replicas)
	for i := range replica {
		endpoints = append(endpoints, clientEndpointForOrdinalIndex(sts, i, tlsConfig))
	}
	return endpoints
}

func areAllMembersHealthy(sts *appsv1.StatefulSet, logger logr.Logger, tlsConfig *tls.Config) (bool, error) {
	_, health, err := healthCheck(sts, logger, tlsConfig)
	if err != nil {
		return false, err
	}

	for _, h := range health {
		if !h.Health {
			return false, nil
		}
	}
	return true, nil
}

// healthCheck returns a memberList and an error.
// If any member (excluding not yet started or already removed member)
// is unhealthy, the error won't be nil.
func healthCheck(sts *appsv1.StatefulSet, lg klog.Logger, tlsConfig *tls.Config) (*clientv3.MemberListResponse, []etcdutils.EpHealth, error) {
	replica := int(*sts.Spec.Replicas)
	if replica == 0 {
		return nil, nil, nil
	}

	endpoints := clientEndpointsFromStatefulsets(sts, tlsConfig)

	memberlistResp, err := etcdutils.MemberList(endpoints, tlsConfig)
	if err != nil {
		return nil, nil, err
	}
	memberCnt := len(memberlistResp.Members)

	// Usually replica should be equal to memberCnt. If it isn't, then
	// it means previous reconcile loop somehow interrupted right after
	// adding (replica < memberCnt) or removing (replica > memberCnt)
	// a member from the cluster. In that case, we shouldn't run health
	// check on the not yet started or already removed member.
	cnt := min(replica, memberCnt)

	lg.Info("health checking", "replica", replica, "len(members)", memberCnt)
	endpoints = endpoints[:cnt]

	healthInfos, err := etcdutils.ClusterHealth(endpoints, tlsConfig)
	if err != nil {
		return memberlistResp, nil, err
	}

	for _, healthInfo := range healthInfos {
		if !healthInfo.Health {
			// TODO: also update metrics?
			return memberlistResp, healthInfos, errors.New(healthInfo.String())
		}
		lg.Info(healthInfo.String())
	}

	return memberlistResp, healthInfos, nil
}

func getClientCertName(etcdClusterName string) string {
	clientCertName := fmt.Sprintf("%s-%s-tls", etcdClusterName, "client")
	return clientCertName
}

func getServerCertName(etcdClusterName string) string {
	serverCertName := fmt.Sprintf("%s-%s-tls", etcdClusterName, "server")
	return serverCertName
}

func getPeerCertName(etcdClusterName string) string {
	peerCertName := fmt.Sprintf("%s-%s-tls", etcdClusterName, "peer")
	return peerCertName
}

func defaultCertificateDNSNames(ec *ecv1alpha1.EtcdCluster) []string {
	clientServiceName := clientServiceNameForEtcdCluster(ec)
	return []string{
		fmt.Sprintf("*.%s.%s.svc", ec.Name, ec.Namespace),
		fmt.Sprintf("*.%s.%s.svc.cluster.local", ec.Name, ec.Namespace),
		fmt.Sprintf("%s.%s.svc", clientServiceName, ec.Namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", clientServiceName, ec.Namespace),
	}
}

func createCMCertificateConfig(ec *ecv1alpha1.EtcdCluster) *certInterface.Config {
	cmConfig := ec.Spec.TLS.ProviderCfg.CertManagerCfg
	duration, err := time.ParseDuration(cmConfig.ValidityDuration)
	if err != nil {
		log.Printf("Failed to parse ValidityDuration: %s", err)
	}

	dnsNames := cmConfig.AltNames.DNSNames
	if dnsNames == nil {
		dnsNames = defaultCertificateDNSNames(ec)
	}
	getAltNames := certInterface.AltNames{
		DNSNames: dnsNames,
		IPs:      cmConfig.AltNames.IPs,
	}

	config := &certInterface.Config{
		CommonName:       cmConfig.CommonName,
		Organization:     cmConfig.Organization,
		ValidityDuration: duration,
		AltNames:         getAltNames,
		ExtraConfig: map[string]any{
			"issuerName": cmConfig.IssuerName,
			"issuerKind": cmConfig.IssuerKind,
		},
	}
	return config
}

func createAutoCertificateConfig(_ *ecv1alpha1.EtcdCluster) *certInterface.Config {
	// TODO
	config := &certInterface.Config{}
	return config
}

func getCertificatesContent(ctx context.Context, c client.Client, ec *ecv1alpha1.EtcdCluster, certName string) (*certInterface.CertificateContent, error) {
	cert, err := certificate.NewProvider(certificate.ProviderType(ec.Spec.TLS.Provider), c, ec)
	if err != nil {
		// TODO: instead of error, set default autoConfig
		return nil, err
	}

	return cert.GetCertificateContent(ctx, certName, ec.Namespace)
}

func getClientCertificate(ctx context.Context, c client.Client, ec *ecv1alpha1.EtcdCluster) (*certInterface.CertificateContent, error) {
	return getCertificatesContent(ctx, c, ec, getClientCertName(ec.Name))
}

func createCertificate(ec *ecv1alpha1.EtcdCluster, ctx context.Context, c client.Client, certName string) error {
	cert, certErr := certificate.NewProvider(certificate.ProviderType(ec.Spec.TLS.Provider), c, ec)
	if certErr != nil {
		// TODO: instead of error, set default autoConfig
		return certErr
	}
	if cert == nil {
		return fmt.Errorf("certificate provider %q is not implemented", ec.Spec.TLS.Provider)
	}

	var config *certInterface.Config
	switch {
	case ec.Spec.TLS.ProviderCfg.AutoCfg != nil:
		config = createAutoCertificateConfig(ec)
	case ec.Spec.TLS.ProviderCfg.CertManagerCfg != nil:
		config = createCMCertificateConfig(ec)
	default:
		return fmt.Errorf("no certificate provider config specified for %s", ec.Name)
	}

	if err := cert.EnsureCertificateSecret(ctx, certName, ec.Namespace, config); err != nil {
		log.Printf("Error ensuring certificate: %s", err)
		return err
	}
	return nil
}

func getTlsConfig(ctx context.Context, c client.Client, ec *ecv1alpha1.EtcdCluster) (*tls.Config, error) {
	if ec.Spec.TLS != nil {
		certs, err := getClientCertificate(ctx, c, ec)
		if err != nil {
			return nil, err
		}

		rootCAs := x509.NewCertPool()
		if ok := rootCAs.AppendCertsFromPEM(certs.CaCertificate); !ok {
			return nil, fmt.Errorf("error create root CA for the DB connector")
		}

		certificate, err := tls.X509KeyPair(certs.Certificate, certs.PrivateKey)
		if err != nil {
			return nil, err
		}

		return &tls.Config{
			RootCAs:      rootCAs,
			Certificates: []tls.Certificate{certificate},
		}, nil
	}

	return nil, nil
}

func createClientCertificate(ctx context.Context, ec *ecv1alpha1.EtcdCluster, c client.Client) error {
	certName := getClientCertName(ec.Name)
	createClientCertErr := createCertificate(ec, ctx, c, certName)
	return createClientCertErr
}

func createServerCertificate(ctx context.Context, ec *ecv1alpha1.EtcdCluster, c client.Client) error {
	serverCertName := getServerCertName(ec.Name)
	createServerCertErr := createCertificate(ec, ctx, c, serverCertName)
	if createServerCertErr != nil {
		return createServerCertErr
	}
	return nil
}

func createPeerCertificate(ctx context.Context, ec *ecv1alpha1.EtcdCluster, c client.Client) error {
	peerCertName := getPeerCertName(ec.Name)
	createPeerCertErr := createCertificate(ec, ctx, c, peerCertName)
	if createPeerCertErr != nil {
		return createPeerCertErr
	}
	return nil
}

func applyEtcdMemberCerts(ctx context.Context, ec *ecv1alpha1.EtcdCluster, c client.Client, logger logr.Logger) error {
	var err error
	if ec.Spec.TLS != nil {
		createServerCertErr := createServerCertificate(ctx, ec, c)
		if createServerCertErr != nil {
			err = createServerCertErr
			logger.Error(createServerCertErr, "Error creating Server Certificate")

		}
		createPeerCertErr := createPeerCertificate(ctx, ec, c)
		if createPeerCertErr != nil {
			err = createPeerCertErr
			logger.Error(createPeerCertErr, "Error creating Peer Certificate")

		}

	}
	return err
}
