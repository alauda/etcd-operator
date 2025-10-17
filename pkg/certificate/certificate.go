package certificate

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ecv1alpha1 "go.etcd.io/etcd-operator/api/v1alpha1"
	certManager "go.etcd.io/etcd-operator/pkg/certificate/cert_manager"
	certInterface "go.etcd.io/etcd-operator/pkg/certificate/interfaces"
)

type ProviderType string

const (
	Auto        ProviderType = "auto"
	CertManager ProviderType = "cert-manager"
	// add more ...
)

func NewProvider(pt ProviderType, c client.Client, ec *ecv1alpha1.EtcdCluster) (certInterface.Provider, error) {
	switch pt {
	case Auto:
		return nil, nil // change me later
	case CertManager:
		return certManager.New(c, ec), nil
	}

	return nil, fmt.Errorf("unknown provider type: %s", pt)
}
