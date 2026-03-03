//go:build otel

package cmd

import (
	"context"
	"log/slog"

	"github.com/nextlevelbuilder/goclaw/pkg/config"
	"github.com/nextlevelbuilder/goclaw/pkg/tracing"
	"github.com/nextlevelbuilder/goclaw/pkg/tracing/otelexport"
)

// initOTelExporter creates and wires the OpenTelemetry OTLP exporter
// when the telemetry config is enabled. Only compiled with -tags otel.
func initOTelExporter(ctx context.Context, cfg *config.Config, collector *tracing.Collector) {
	if collector == nil {
		return
	}
	if !cfg.Telemetry.Enabled || cfg.Telemetry.Endpoint == "" {
		slog.Debug("OTel export available but not enabled (set telemetry.enabled + telemetry.endpoint)")
		return
	}

	otelExp, err := otelexport.New(ctx, otelexport.Config{
		Endpoint:    cfg.Telemetry.Endpoint,
		Protocol:    cfg.Telemetry.Protocol,
		Insecure:    cfg.Telemetry.Insecure,
		ServiceName: cfg.Telemetry.ServiceName,
		Headers:     cfg.Telemetry.Headers,
	})
	if err != nil {
		slog.Warn("failed to create OTel exporter", "error", err)
		return
	}

	collector.SetExporter(otelExp)
	slog.Info("OpenTelemetry OTLP export enabled",
		"endpoint", cfg.Telemetry.Endpoint,
		"protocol", cfg.Telemetry.Protocol,
	)
}
