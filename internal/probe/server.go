package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"go.etcd.io/etcd-operator/internal/etcdutils"
)

var ErrLearner = errors.New("member is learner")

type Checker interface {
	CheckReady(ctx context.Context) error
}

type EtcdChecker struct {
	Endpoint  string
	TLSConfig *tls.Config
	Timeout   time.Duration
}

func (c EtcdChecker) CheckReady(ctx context.Context) error {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	health := etcdutils.EndpointHealth(ctx, c.Endpoint, c.TLSConfig)
	if !health.Health {
		if health.Error != "" {
			return fmt.Errorf("etcd endpoint is not healthy: %s", health.Error)
		}
		return errors.New("etcd endpoint is not healthy")
	}
	if health.Status == nil {
		return errors.New("etcd status is unavailable")
	}
	if health.Status.IsLearner {
		return ErrLearner
	}
	return nil
}

type Server struct {
	checker Checker
}

func NewServer(checker Checker) *Server {
	return &Server{checker: checker}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/readyz", s.readyz)
	return mux
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.checker.CheckReady(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ready\n"))
}

func LoadTLSConfig(caFile, certFile, keyFile string) (*tls.Config, error) {
	if caFile == "" && certFile == "" && keyFile == "" {
		return nil, nil
	}
	if caFile == "" || certFile == "" || keyFile == "" {
		return nil, errors.New("cacert, cert, and key must be provided together")
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	rootCAs := x509.NewCertPool()
	if ok := rootCAs.AppendCertsFromPEM(caPEM); !ok {
		return nil, errors.New("failed to append CA certificate")
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		RootCAs:      rootCAs,
		Certificates: []tls.Certificate{cert},
	}, nil
}
