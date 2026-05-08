// Package observability wires OpenTelemetry metrics with a Prometheus exporter
// and exposes a /metrics HTTP endpoint for scraping.
package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/sdk/metric"
)

// MetricsServer serves the OTel Prometheus /metrics endpoint and installs a
// global MeterProvider backed by the Prometheus exporter. Call Start to begin
// serving; call Stop to drain the HTTP server and shut down the SDK.
type MetricsServer struct {
	provider *metric.MeterProvider
	httpSrv  *http.Server
	logger   *slog.Logger
}

// NewMetricsServer creates a MetricsServer that will listen on addr (e.g. ":9090").
// It registers a global OTel MeterProvider backed by a Prometheus exporter so
// any code using otel/metric.Global() will have its measurements exposed at
// http://<addr>/metrics.
func NewMetricsServer(addr string, logger *slog.Logger) (*MetricsServer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	// Use a dedicated registry so the /metrics endpoint is isolated from any
	// other default-registry metrics (e.g. Go runtime metrics from test helpers).
	reg := prometheus.NewRegistry()
	exporter, err := otelprometheus.New(otelprometheus.WithRegisterer(reg))
	if err != nil {
		return nil, fmt.Errorf("observability: create prometheus exporter: %w", err)
	}
	provider := metric.NewMeterProvider(metric.WithReader(exporter))

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	return &MetricsServer{
		provider: provider,
		httpSrv: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
		logger: logger,
	}, nil
}

// Provider returns the OTel MeterProvider. Pass this to otel.SetMeterProvider
// after construction.
func (s *MetricsServer) Provider() *metric.MeterProvider { return s.provider }

// Start begins listening in a background goroutine. Returns immediately; errors
// from Serve are logged but do not block.
func (s *MetricsServer) Start() error {
	lis, err := net.Listen("tcp", s.httpSrv.Addr)
	if err != nil {
		return fmt.Errorf("observability: listen %s: %w", s.httpSrv.Addr, err)
	}
	s.logger.Info("observability: metrics endpoint started", "addr", lis.Addr().String())
	go func() {
		if err := s.httpSrv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("observability: metrics server error", "error", err)
		}
	}()
	return nil
}

// Stop shuts down the HTTP server and flushes any pending metric data.
func (s *MetricsServer) Stop(ctx context.Context) {
	if err := s.httpSrv.Shutdown(ctx); err != nil {
		s.logger.Warn("observability: metrics server shutdown error", "error", err)
	}
	if err := s.provider.Shutdown(ctx); err != nil {
		s.logger.Warn("observability: meter provider shutdown error", "error", err)
	}
}
