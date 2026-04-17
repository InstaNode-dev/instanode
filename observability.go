package main

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

// initObservability sets up OpenTelemetry tracing based on config.
// The application code uses OTel APIs exclusively — swapping backends
// (New Relic, Datadog, Grafana Cloud, Jaeger, etc.) is a config change only.
//
// Returns a shutdown function that must be called on application exit.
func initObservability(cfg *Config) func(context.Context) {
	if !cfg.Observability.Enabled {
		slog.Info("observability: disabled")
		return func(ctx context.Context) {}
	}

	ctx := context.Background()

	// Build the resource (identifies this service in the backend).
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.Observability.ServiceName),
			attribute.String("deployment.environment", cfg.Observability.Environment),
		),
		resource.WithTelemetrySDK(),
		resource.WithHost(),
	)
	if err != nil {
		slog.Error("observability: failed to create resource", "error", err)
		return func(ctx context.Context) {}
	}

	// Create the trace exporter based on config.
	var exporter sdktrace.SpanExporter
	switch cfg.Observability.Exporter {
	case "otlp":
		opts := []otlptracehttp.Option{
			otlptracehttp.WithEndpoint(cfg.Observability.OTLPEndpoint),
		}
		if cfg.Observability.OTLPInsecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		if len(cfg.Observability.OTLPHeaders) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(cfg.Observability.OTLPHeaders))
		}
		exporter, err = otlptracehttp.New(ctx, opts...)
		if err != nil {
			slog.Error("observability: failed to create OTLP exporter", "error", err)
			return func(ctx context.Context) {}
		}
		slog.Info("observability: OTLP exporter initialized", "endpoint", cfg.Observability.OTLPEndpoint)

	case "stdout":
		exporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			slog.Error("observability: failed to create stdout exporter", "error", err)
			return func(ctx context.Context) {}
		}
		slog.Info("observability: stdout exporter initialized")

	default:
		slog.Warn("observability: unknown exporter, disabling", "exporter", cfg.Observability.Exporter)
		return func(ctx context.Context) {}
	}

	// Configure the trace provider with batched exports.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(cfg.Observability.SampleRate)),
	)

	// Register as the global tracer provider.
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	slog.Info("observability: tracing enabled",
		"service", cfg.Observability.ServiceName,
		"exporter", cfg.Observability.Exporter,
		"sample_rate", cfg.Observability.SampleRate,
	)

	return func(ctx context.Context) {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(shutdownCtx); err != nil {
			slog.Error("observability: shutdown failed", "error", err)
		}
	}
}

// appTracer returns a tracer scoped to the application.
func appTracer() trace.Tracer {
	return otel.Tracer("instant-lite-api")
}
