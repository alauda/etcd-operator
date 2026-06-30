package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.etcd.io/etcd-operator/internal/probe"
)

func main() {
	var listenAddress string
	var endpoint string
	var timeout time.Duration
	var caFile string
	var certFile string
	var keyFile string

	flag.StringVar(&listenAddress, "listen-address", ":9980", "The address the etcd probe endpoint binds to.")
	flag.StringVar(&endpoint, "endpoint", "", "The local etcd endpoint to check.")
	flag.DurationVar(&timeout, "timeout", 30*time.Second, "Timeout for each readyz check.")
	flag.StringVar(&caFile, "cacert", "", "Path to the CA certificate for TLS etcd checks.")
	flag.StringVar(&certFile, "cert", "", "Path to the client certificate for TLS etcd checks.")
	flag.StringVar(&keyFile, "key", "", "Path to the client key for TLS etcd checks.")
	flag.Parse()

	if endpoint == "" {
		fmt.Fprintln(os.Stderr, "--endpoint is required")
		os.Exit(2)
	}

	tlsConfig, err := probe.LoadTLSConfig(caFile, certFile, keyFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load TLS config: %v\n", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr: listenAddress,
		Handler: probe.NewServer(probe.EtcdChecker{
			Endpoint:  endpoint,
			TLSConfig: tlsConfig,
			Timeout:   timeout,
		}).Handler(),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-signalCh:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "failed to shut down after %s: %v\n", sig, err)
			os.Exit(1)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "probe server failed: %v\n", err)
			os.Exit(1)
		}
	}
}
