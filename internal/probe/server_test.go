package probe

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeChecker struct {
	err    error
	called bool
	ctx    context.Context
}

func (f *fakeChecker) CheckReady(ctx context.Context) error {
	f.called = true
	f.ctx = ctx
	return f.err
}

func performProbeRequest(handler http.Handler, path string, ctx context.Context) *httptest.ResponseRecorder {
	if ctx == nil {
		ctx = context.Background()
	}
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestServerHealthzAlwaysOK(t *testing.T) {
	checker := &fakeChecker{err: errors.New("etcd down")}
	server := NewServer(checker)

	rec := performProbeRequest(server.Handler(), "/healthz", nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok\n", rec.Body.String())
	assert.False(t, checker.called)
}

func TestServerReadyzOK(t *testing.T) {
	checker := &fakeChecker{}
	server := NewServer(checker)

	rec := performProbeRequest(server.Handler(), "/readyz", nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ready\n", rec.Body.String())
	assert.True(t, checker.called)
}

func TestServerReadyzUnavailableOnCheckerError(t *testing.T) {
	checker := &fakeChecker{err: errors.New("not ready")}
	server := NewServer(checker)

	rec := performProbeRequest(server.Handler(), "/readyz", nil)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "not ready")
	assert.True(t, checker.called)
}

func TestServerReadyzUnavailableForLearner(t *testing.T) {
	checker := &fakeChecker{err: ErrLearner}
	server := NewServer(checker)

	rec := performProbeRequest(server.Handler(), "/readyz", nil)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "member is learner")
}

func TestServerReadyzPassesRequestContext(t *testing.T) {
	type contextKey string
	const key contextKey = "request-id"
	checker := &fakeChecker{}
	server := NewServer(checker)
	ctx := context.WithValue(context.Background(), key, "abc")

	rec := performProbeRequest(server.Handler(), "/readyz", ctx)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, checker.ctx)
	assert.Equal(t, "abc", checker.ctx.Value(key))
}

func TestLoadTLSConfigEmpty(t *testing.T) {
	cfg, err := LoadTLSConfig("", "", "")

	require.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestLoadTLSConfigRequiresAllFiles(t *testing.T) {
	tests := []struct {
		name string
		ca   string
		cert string
		key  string
	}{
		{name: "only ca", ca: "ca.crt"},
		{name: "only cert", cert: "tls.crt"},
		{name: "only key", key: "tls.key"},
		{name: "missing key", ca: "ca.crt", cert: "tls.crt"},
		{name: "missing cert", ca: "ca.crt", key: "tls.key"},
		{name: "missing ca", cert: "tls.crt", key: "tls.key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadTLSConfig(tt.ca, tt.cert, tt.key)

			assert.Nil(t, cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be provided together")
		})
	}
}

func TestLoadTLSConfigInvalidCA(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(caFile, []byte("not pem"), 0o600))
	require.NoError(t, os.WriteFile(certFile, []byte("not cert"), 0o600))
	require.NoError(t, os.WriteFile(keyFile, []byte("not key"), 0o600))

	cfg, err := LoadTLSConfig(caFile, certFile, keyFile)

	assert.Nil(t, cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to append CA certificate")
}

func TestLoadTLSConfigValid(t *testing.T) {
	caFile, certFile, keyFile := writeTestTLSFiles(t)

	cfg, err := LoadTLSConfig(caFile, certFile, keyFile)

	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.NotNil(t, cfg.RootCAs)
	require.Len(t, cfg.Certificates, 1)
}

func writeTestTLSFiles(t *testing.T) (string, string, string) {
	t.Helper()

	dir := t.TempDir()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)

	clientKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	require.NoError(t, err)

	caFile := filepath.Join(dir, "ca.crt")
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600))
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(clientKey)}), 0o600))
	return caFile, certFile, keyFile
}
