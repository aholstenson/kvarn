// Package metrics wires the orchestrator's OpenTelemetry metrics pipeline.
//
// Setup builds a MeterProvider that exports to an OTLP gRPC collector when
// enabled, and a no-op provider otherwise. The returned meter is passed
// explicitly into the components that publish instruments — no globals — so
// tests can substitute a no-op meter without touching otel state.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect/v2"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config controls metrics setup. Endpoint follows OTel conventions: empty +
// Enabled means the SDK reads OTEL_EXPORTER_OTLP_ENDPOINT itself.
type Config struct {
	Enabled     bool
	Endpoint    string // optional override; passes through to otlpmetricgrpc
	ServiceName string // resource attribute service.name
	Version     string // resource attribute service.version
}

// Setup builds the meter provider for cfg. The returned Meter is always
// non-nil; the shutdown func is a no-op when metrics are disabled or setup
// fails (we log and continue rather than refusing to start the orchestrator
// because the collector is unreachable). It honors standard OTEL_* env vars.
func Setup(ctx context.Context, cfg Config) (metric.Meter, func(context.Context) error, error) {
	noopShutdown := func(context.Context) error { return nil }
	if !cfg.Enabled {
		return noop.NewMeterProvider().Meter("kvarn"), noopShutdown, nil
	}

	opts := []otlpmetricgrpc.Option{}
	if cfg.Endpoint != "" {
		opts = append(opts, otlpmetricgrpc.WithEndpoint(cfg.Endpoint), otlpmetricgrpc.WithInsecure())
	}
	exporter, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("create otlp metrics exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.Version),
	))
	if err != nil {
		_ = exporter.Shutdown(context.Background())
		return nil, nil, fmt.Errorf("build otel resource: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(30*time.Second))),
	)
	return provider.Meter("kvarn"), provider.Shutdown, nil
}

// NewRPCInterceptor builds a server interceptor that records
// kvarn.rpc.duration_seconds for every handled call, tagged with procedure and
// connect status code. A nil meter is tolerated and yields a no-op recorder
// (used by tests / when metrics are off).
func NewRPCInterceptor(m metric.Meter) (connect.ServerInterceptor, error) {
	if m == nil {
		m = noop.NewMeterProvider().Meter("kvarn")
	}
	duration, err := m.Float64Histogram(
		"kvarn.rpc.duration_seconds",
		metric.WithDescription("ConnectRPC handler duration in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			start := time.Now()
			err := next(ctx, spec, stream)
			duration.Record(ctx, time.Since(start).Seconds(),
				metric.WithAttributes(
					attrStr("procedure", spec.Procedure),
					attrStr("code", codeOf(err)),
				),
			)
			return err
		}
	}, nil
}

// codeOf maps a handler error to its connect code string, or "ok" when nil.
func codeOf(err error) string {
	if err == nil {
		return "ok"
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return cerr.Code().String()
	}
	return connect.CodeUnknown.String()
}

// LogStartupError logs an OTel setup failure consistently from callers. We
// don't want metrics misconfiguration to kill the orchestrator, so callers
// fall back to a no-op meter on error and log via this helper.
func LogStartupError(err error) {
	slog.Warn("OpenTelemetry metrics disabled: setup failed", "error", err)
}
