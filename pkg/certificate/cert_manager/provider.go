package cert_manager

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"reflect"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	ecv1alpha1 "go.etcd.io/etcd-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	interfaces "go.etcd.io/etcd-operator/pkg/certificate/interfaces"
)

const (
	IssuerNameKey = "issuerName"
	IssuerKindKey = "issuerKind"
)

type CertManagerProvider struct {
	client.Client
	etcdCluster *ecv1alpha1.EtcdCluster
}

var _ interfaces.Provider = (*CertManagerProvider)(nil)

func New(c client.Client, ec *ecv1alpha1.EtcdCluster) interfaces.Provider {
	return &CertManagerProvider{
		c,
		ec,
	}
}

func (cm *CertManagerProvider) EnsureCertificateSecret(ctx context.Context, secretName, namespace string,
	cfg *interfaces.Config) error {
	cmCertificate := &certmanagerv1.Certificate{}
	err := cm.Get(ctx, client.ObjectKey{Name: secretName, Namespace: namespace}, cmCertificate)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			if valErr := cm.validateCertificateConfig(ctx, namespace, cfg); valErr != nil {
				return valErr
			}
			return cm.createCertificate(ctx, secretName, namespace, cfg)
		}
		return err
	}

	if valErr := cm.validateCertificateConfig(ctx, namespace, cfg); valErr != nil {
		return valErr
	}
	desiredSpec, err := certificateSpecForConfig(secretName, cfg)
	if err != nil {
		return err
	}
	if !certificateSpecEqual(cmCertificate.Spec, desiredSpec) {
		cmCertificate.Spec = desiredSpec
		if err := cm.Update(ctx, cmCertificate); err != nil {
			return err
		}
		return nil
	}

	log.Printf("Valid certificate: %s present in namespace: %s, checking certificate status...", secretName, namespace)

	err = cm.checkCertificateStatus(secretName, namespace, ctx)
	if err != nil && !errors.Is(err, interfaces.ErrUnknown) {
		for try := range interfaces.MaxRetries {
			// Wait for the certificate to be in "Ready" state
			// Reference: https://cert-manager.io/docs/usage/certificate/#inner-workings-diagram-for-developers
			log.Printf("Certificate Status: retry attempt %v, after %v, error: %v", try, interfaces.RetryInterval, err)
			time.Sleep(interfaces.RetryInterval)
			err = cm.checkCertificateStatus(secretName, namespace, ctx)
			if err == nil {
				break
			}
		}
		if err != nil {
			return err
		}
	} else {
		return err
	}

	log.Printf("Certificate Status: %s ready in namespace: %s, validating associated secret...", secretName, namespace)

	err = cm.ValidateCertificateSecret(ctx, secretName, namespace, cfg)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return err
		} else {
			return fmt.Errorf("invalid certificate secret: %s present in namespace: %s, please delete and try again.\nError: %s",
				secretName, namespace, err)
		}
	}

	log.Printf("Valid certificate secret: %s already present in namespace: %s", secretName, namespace)
	return nil
}

func (cm *CertManagerProvider) GetCertificateContent(ctx context.Context, secretName, namespace string) (*interfaces.CertificateContent, error) {
	secret := &corev1.Secret{}
	err := cm.Get(ctx, client.ObjectKey{Name: secretName, Namespace: namespace}, secret)
	if err != nil {
		return nil, err
	}

	caCertificateData, exists := secret.Data["ca.crt"]
	if !exists {
		return nil, interfaces.ErrTLSCert
	}

	certificateData, exists := secret.Data["tls.crt"]
	if !exists {
		return nil, interfaces.ErrTLSCert
	}

	privateKeyData, keyExists := secret.Data["tls.key"]
	if !keyExists {
		return nil, interfaces.ErrTLSKey
	}

	return &interfaces.CertificateContent{
		CaCertificate: caCertificateData,
		Certificate:   certificateData,
		PrivateKey:    privateKeyData,
	}, nil
}

func (cm *CertManagerProvider) ValidateCertificateSecret(ctx context.Context, secretName, namespace string,
	_ *interfaces.Config) error {
	var err error
	secret := &corev1.Secret{}
	err = cm.Get(ctx, client.ObjectKey{Name: secretName, Namespace: namespace}, secret)
	if err != nil && k8serrors.IsNotFound(err) {
		for try := range interfaces.MaxRetries {
			// Wait for cert-manager reconciler to create the associated certificate secret
			// Reference: https://cert-manager.io/docs/usage/certificate/#inner-workings-diagram-for-developers
			log.Printf("Valid certificate secret: retry attempt %v, after %v, error: %v", try+1, interfaces.RetryInterval, err)
			time.Sleep(interfaces.RetryInterval)
			err = cm.Get(ctx, client.ObjectKey{Name: secretName, Namespace: namespace}, secret)
			if err == nil {
				break
			}
		}
		if err != nil {
			return err
		}
	} else {
		return err
	}

	certificateData, exists := secret.Data["tls.crt"]
	if !exists {
		return interfaces.ErrTLSCert
	}

	decodeCertificatePem, _ := pem.Decode(certificateData)
	if decodeCertificatePem == nil {
		return interfaces.ErrDecodeCert
	}

	privateKeyData, keyExists := secret.Data["tls.key"]
	if !keyExists {
		return interfaces.ErrTLSKey
	}

	parseCert, err := x509.ParseCertificate(decodeCertificatePem.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse certificate: %w", err)
	}

	if parseCert.NotAfter.Before(time.Now()) {
		return interfaces.ErrCertExpired
	}

	privateKey, err := parsePrivateKey(privateKeyData)
	if err != nil {
		return fmt.Errorf("failed to parse private key: %w", err)
	}

	if checkKeyPairErr := checkKeyPair(parseCert, privateKey); checkKeyPairErr != nil {
		return fmt.Errorf("private key does not match certificate: %w", checkKeyPairErr)
	}

	return nil
}

func (cm *CertManagerProvider) DeleteCertificateSecret(ctx context.Context, secretName, namespace string) error {
	cmCertificate := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
	}
	certStatusErr := cm.checkCertificateStatus(secretName, namespace, ctx)
	if certStatusErr != nil {
		log.Printf("Certificate associated not ready yet, try again later.")
		return certStatusErr
	}

	dErr := cm.Delete(ctx, cmCertificate)
	if dErr != nil {
		return dErr
	}

	// By default, cert-manager Certificate deletion does not delete the associated secret.
	// Existing secret will allow to services relying on that Certificate, so additionally delete it
	// More info: https://cert-manager.io/docs/usage/certificate/#cleaning-up-secrets-when-certificates-are-deleted
	cmSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
	}
	dSecretErr := cm.Delete(ctx, cmSecret)
	if dSecretErr != nil {
		if k8serrors.IsNotFound(dSecretErr) {
			fmt.Println("Certificate secret not found, maybe already deleted")
		} else {
			return dSecretErr
		}

	}

	return nil
}

// RevokeCertificate is not supported, certificates can only be deleted which is handled by DeleteCertificateSecret
// as per official documentation: https://cert-manager.io/docs/usage/certificate/#inner-workings-diagram-for-developers
func (cm *CertManagerProvider) RevokeCertificate(ctx context.Context, secretName string, namespace string) error {
	return nil
}

func (cm *CertManagerProvider) GetCertificateConfig(ctx context.Context,
	secretName, namespace string) (*interfaces.Config, error) {
	cmCertificate := &certmanagerv1.Certificate{}
	err := cm.Get(ctx, client.ObjectKey{Name: secretName, Namespace: namespace}, cmCertificate)
	if err != nil {
		return nil, fmt.Errorf("failed to get certificate: %w", err)
	}

	var ipAddresses []net.IP
	if len(cmCertificate.Spec.IPAddresses) != 0 {
		ipAddresses = make([]net.IP, 0, len(cmCertificate.Spec.IPAddresses))
		for _, ipAddress := range cmCertificate.Spec.IPAddresses {
			if ip := net.ParseIP(ipAddress); ip != nil {
				ipAddresses = append(ipAddresses, ip)
			}
		}
	}

	cfg := &interfaces.Config{
		CommonName:   cmCertificate.Spec.CommonName,
		Organization: cmCertificate.Spec.Subject.Organizations,
		AltNames: interfaces.AltNames{
			DNSNames: cmCertificate.Spec.DNSNames,
			IPs:      ipAddresses,
		},
		ValidityDuration: cmCertificate.Spec.Duration.Duration,
		ExtraConfig: map[string]any{
			IssuerNameKey: cmCertificate.Spec.IssuerRef.Name,
			IssuerKindKey: cmCertificate.Spec.IssuerRef.Kind,
		},
	}

	return cfg, nil
}

// checkCertificateStatus returns the current status of the certificate creation
func (cm *CertManagerProvider) checkCertificateStatus(certificateName, namespace string, ctx context.Context) error {
	cmCertificate := &certmanagerv1.Certificate{}
	err := cm.Get(ctx, client.ObjectKey{Name: certificateName, Namespace: namespace}, cmCertificate)
	if err != nil {
		return err
	}
	cmStatus := cmCertificate.Status.Conditions
	for _, condition := range cmStatus {
		switch condition.Type {
		case certmanagerv1.CertificateConditionReady:
			log.Printf("Certificate Ready: %v (Reason: %s, Message: %s)\n",
				condition.Status, condition.Reason, condition.Message)
			return nil
		case certmanagerv1.CertificateConditionIssuing:
			return fmt.Errorf("certificate Issuing: %v (Reason: %s, Message: %s), error: %w",
				condition.Status, condition.Reason, condition.Message, interfaces.ErrPending)
		default:
			return fmt.Errorf("certificate status unknown: %v (Reason: %s, Message: %s), error: %w",
				condition.Status, condition.Reason, condition.Message, interfaces.ErrUnknown)
		}
	}
	return nil
}

// checkIssuerExists checks for if the provided issuer is present in the namespace/cluster
func (cm *CertManagerProvider) checkIssuerExists(issuerName, issuerKind, namespace string, ctx context.Context) error {
	switch issuerKind {
	case "Issuer":
		issuer := &certmanagerv1.Issuer{}
		err := cm.Get(ctx, client.ObjectKey{Name: issuerName, Namespace: namespace}, issuer)
		if k8serrors.IsNotFound(err) {
			return fmt.Errorf("issuer %s not found in namespace %s", issuerName, namespace)
		}
	case "ClusterIssuer":
		clusterIssuer := &certmanagerv1.ClusterIssuer{}
		err := cm.Get(ctx, client.ObjectKey{Name: issuerName}, clusterIssuer)
		if k8serrors.IsNotFound(err) {
			return fmt.Errorf("clusterIssuer %s not found", issuerName)
		}
	default:
		return fmt.Errorf("unsupported issuer kind: %s", issuerKind)
	}
	return nil
}

// validateCertificateConfig checks if the config passed is valid
func (cm *CertManagerProvider) validateCertificateConfig(ctx context.Context, namespace string,
	cfg *interfaces.Config) error {
	issuerName, isValid := cfg.ExtraConfig[IssuerNameKey].(string)
	if !isValid {
		return fmt.Errorf("value for %s not correctly provided, try again", IssuerNameKey)
	}
	issuerKind, isValid := cfg.ExtraConfig[IssuerKindKey].(string)
	if !isValid {
		return fmt.Errorf("value for %s not correctly provided, try again", IssuerKindKey)
	}
	checkIssuerExist := cm.checkIssuerExists(issuerName, issuerKind, namespace, ctx)
	if checkIssuerExist != nil {
		return checkIssuerExist
	}
	return nil
}

func certificateSpecForConfig(secretName string, cfg *interfaces.Config) (certmanagerv1.CertificateSpec, error) {
	issuerName, isValid := cfg.ExtraConfig[IssuerNameKey].(string)
	if !isValid {
		return certmanagerv1.CertificateSpec{}, fmt.Errorf("value for %s not correctly provided, try again", IssuerNameKey)
	}
	issuerKind, isValid := cfg.ExtraConfig[IssuerKindKey].(string)
	if !isValid {
		return certmanagerv1.CertificateSpec{}, fmt.Errorf("value for %s not correctly provided, try again", IssuerKindKey)
	}

	return certmanagerv1.CertificateSpec{
		CommonName: cfg.CommonName,
		Subject: &certmanagerv1.X509Subject{
			Organizations: cfg.Organization,
		},
		SecretName:  secretName,
		DNSNames:    cfg.AltNames.DNSNames,
		IPAddresses: ipStrings(cfg.AltNames.IPs),
		IssuerRef: cmmeta.ObjectReference{
			Name: issuerName,
			Kind: issuerKind,
		},
		Duration: &metav1.Duration{Duration: cfg.ValidityDuration},
	}, nil
}

func ipStrings(ips []net.IP) []string {
	if len(ips) == 0 {
		return nil
	}
	result := make([]string, 0, len(ips))
	for _, ip := range ips {
		if ip != nil {
			result = append(result, ip.String())
		}
	}
	return result
}

func certificateSpecEqual(current, desired certmanagerv1.CertificateSpec) bool {
	return current.CommonName == desired.CommonName &&
		reflect.DeepEqual(current.Subject, desired.Subject) &&
		current.SecretName == desired.SecretName &&
		reflect.DeepEqual(current.DNSNames, desired.DNSNames) &&
		reflect.DeepEqual(current.IPAddresses, desired.IPAddresses) &&
		current.IssuerRef.Name == desired.IssuerRef.Name &&
		current.IssuerRef.Kind == desired.IssuerRef.Kind &&
		reflect.DeepEqual(current.Duration, desired.Duration)
}

// createCertificate creates a cert-manager Certificate resource in the specified namespace.
// returns an error if the Certificate resource cannot be created.
func (cm *CertManagerProvider) createCertificate(ctx context.Context, secretName, namespace string,
	cfg *interfaces.Config) error {
	certificateSpec, err := certificateSpecForConfig(secretName, cfg)
	if err != nil {
		return err
	}

	certificateResource := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
		},
		Spec: certificateSpec,
	}

	if err := controllerutil.SetControllerReference(cm.etcdCluster, certificateResource, cm.Scheme()); err != nil {
		return err
	}

	return cm.Create(ctx, certificateResource)
}

// parsePrivateKey parses the private key from the PEM-encoded data.
func parsePrivateKey(privateKeyData []byte) (crypto.PrivateKey, error) {
	block, _ := pem.Decode(privateKeyData)
	if block == nil {
		return nil, errors.New("failed to decode private key: invalid PEM")
	}

	// Parse the private key from the PEM block. cert-manager may create
	// PKCS#8, PKCS#1 RSA, or SEC1 EC private keys depending on issuer/key
	// configuration.
	privateKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err == nil {
		return privateKey, nil
	}

	if rsaKey, rsaErr := x509.ParsePKCS1PrivateKey(block.Bytes); rsaErr == nil {
		return rsaKey, nil
	}

	if ecKey, ecErr := x509.ParseECPrivateKey(block.Bytes); ecErr == nil {
		return ecKey, nil
	}

	return nil, fmt.Errorf("failed to parse private key: %w", err)
}

// checkKeyPair checks if the private key matches the certificate by validating the public key
func checkKeyPair(cert *x509.Certificate, privateKey crypto.PrivateKey) error {
	switch key := privateKey.(type) {
	case *rsa.PrivateKey:
		pub, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok || !key.PublicKey.Equal(pub) {
			return interfaces.ErrRSAKeyPair
		}
	case *ecdsa.PrivateKey:
		pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok || !key.PublicKey.Equal(pub) {
			return interfaces.ErrECDSAKeyPair
		}
	case ed25519.PrivateKey:
		pub, ok := cert.PublicKey.(ed25519.PublicKey)
		if !ok || !bytes.Equal(key.Public().(ed25519.PublicKey), pub) {
			return interfaces.ErrED25519KeyPair
		}
	default:
		return fmt.Errorf("unsupported private key type: %T", key)
	}

	return nil
}
