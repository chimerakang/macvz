package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/chimerakang/macvz/pkg/config"
)

func TestFetchLiveDiagnosticsAcceptsNotReadyReport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz/diagnostics" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("Node mac-a: NOT READY\n"))
	}))
	defer srv.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	cfg := config.Default()
	cfg.Node.KubeletPort = int32(n)
	cfg.Node.ServingTLSCertFile = ""
	cfg.Node.ServingTLSKeyFile = ""

	got, err := fetchLiveDiagnostics(context.Background(), cfg, host)
	if err != nil {
		t.Fatalf("fetchLiveDiagnostics: %v", err)
	}
	if !strings.Contains(string(got), "NOT READY") {
		t.Fatalf("diagnostics body = %q, want not-ready report", got)
	}
}
