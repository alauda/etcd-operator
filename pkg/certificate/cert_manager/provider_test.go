package cert_manager

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ecv1alpha1 "go.etcd.io/etcd-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	interfaces "go.etcd.io/etcd-operator/pkg/certificate/interfaces"
)

func testCertificateScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, certmanagerv1.AddToScheme(scheme))
	require.NoError(t, ecv1alpha1.AddToScheme(scheme))
	return scheme
}

func testCertificateConfig() *interfaces.Config {
	return &interfaces.Config{
		CommonName:       "test-etcd",
		Organization:     []string{"test-org"},
		AltNames:         interfaces.AltNames{DNSNames: []string{"test-etcd.default.svc"}, IPs: []net.IP{net.ParseIP("10.0.0.1")}},
		ValidityDuration: time.Hour,
		ExtraConfig: map[string]any{
			IssuerNameKey: "test-issuer",
			IssuerKindKey: "Issuer",
		},
	}
}

func testIssuer() *certmanagerv1.Issuer {
	return &certmanagerv1.Issuer{ObjectMeta: metav1.ObjectMeta{Name: "test-issuer", Namespace: "default"}}
}

func TestEnsureCertificateSecretCreatesCertificate(t *testing.T) {
	ctx := t.Context()
	scheme := testCertificateScheme(t)
	ec := &ecv1alpha1.EtcdCluster{ObjectMeta: metav1.ObjectMeta{Name: "test-etcd", Namespace: "default", UID: "1"}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(testIssuer()).Build()
	provider := New(fakeClient, ec)

	err := provider.EnsureCertificateSecret(ctx, "test-etcd-server-tls", "default", testCertificateConfig())
	assert.NoError(t, err)

	cert := &certmanagerv1.Certificate{}
	err = fakeClient.Get(ctx, client.ObjectKey{Name: "test-etcd-server-tls", Namespace: "default"}, cert)
	assert.NoError(t, err)
	assert.Equal(t, "test-etcd", cert.Spec.CommonName)
	assert.Equal(t, []string{"test-etcd.default.svc"}, cert.Spec.DNSNames)
	assert.Equal(t, []string{"10.0.0.1"}, cert.Spec.IPAddresses)
	assert.Equal(t, "test-issuer", cert.Spec.IssuerRef.Name)
	assert.Equal(t, "Issuer", cert.Spec.IssuerRef.Kind)
	assert.True(t, metav1.IsControlledBy(cert, ec))
}

func TestEnsureCertificateSecretUpdatesCertificateSpecDrift(t *testing.T) {
	ctx := t.Context()
	scheme := testCertificateScheme(t)
	ec := &ecv1alpha1.EtcdCluster{ObjectMeta: metav1.ObjectMeta{Name: "test-etcd", Namespace: "default", UID: "1"}}
	existing := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-etcd-server-tls", Namespace: "default"},
		Spec: certmanagerv1.CertificateSpec{
			CommonName:  "old",
			SecretName:  "test-etcd-server-tls",
			DNSNames:    []string{"old.default.svc"},
			IPAddresses: []string{"10.0.0.2"},
			IssuerRef:   cmmeta.ObjectReference{Name: "test-issuer", Kind: "Issuer"},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(testIssuer(), existing).Build()
	provider := New(fakeClient, ec)

	err := provider.EnsureCertificateSecret(ctx, "test-etcd-server-tls", "default", testCertificateConfig())
	assert.NoError(t, err)

	cert := &certmanagerv1.Certificate{}
	err = fakeClient.Get(ctx, client.ObjectKey{Name: "test-etcd-server-tls", Namespace: "default"}, cert)
	assert.NoError(t, err)
	assert.Equal(t, "test-etcd", cert.Spec.CommonName)
	assert.Equal(t, []string{"test-etcd.default.svc"}, cert.Spec.DNSNames)
	assert.Equal(t, []string{"10.0.0.1"}, cert.Spec.IPAddresses)
	require.NotNil(t, cert.Spec.Duration)
	assert.Equal(t, time.Hour, cert.Spec.Duration.Duration)
}

func TestParsePrivateKeySupportsSEC1ECPrivateKey(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	pemData := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})

	parsed, err := parsePrivateKey(pemData)

	require.NoError(t, err)
	parsedKey, ok := parsed.(*ecdsa.PrivateKey)
	require.True(t, ok)
	assert.True(t, key.Equal(parsedKey))
}

func TestCheckKeyPairSupportsEd25519PrivateKey(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	cert := &x509.Certificate{PublicKey: pub}

	err = checkKeyPair(cert, key)

	assert.NoError(t, err)
}
