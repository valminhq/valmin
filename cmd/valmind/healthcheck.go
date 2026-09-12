package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/valminhq/valmin/internal/config"
)

// healthcheckTimeout bounds the probe. It is the container health check's own budget, so it
// must expire well inside Docker's interval rather than hang a probe that has its answer.
const healthcheckTimeout = 3 * time.Second

// runHealthcheck is the image's HEALTHCHECK: one request to the liveness probe of 11 §10,
// which touches neither the database nor Docker. It ships as a subcommand because the
// alternative is a shell and an HTTP client in the runtime image for one request.
//
// The listen address comes from the same configuration the daemon read, so a panel moved to
// another port is probed on that port without a second place to change it.
func runHealthcheck(ctx context.Context, getenv func(string) string) error {
	cfg, err := config.Load(nil, getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()

	url := "http://" + probeAddr(cfg.Server.Listen) + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("build probe request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe %s: %s", url, resp.Status)
	}
	return nil
}

// probeAddr turns a listen address into one that can be dialled. A wildcard host is what the
// daemon binds and not somewhere to connect: the probe runs inside the container, so it asks
// loopback for what every interface is answering.
func probeAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
