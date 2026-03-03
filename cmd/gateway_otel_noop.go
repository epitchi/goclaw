//go:build !otel

package cmd

import (
	"context"

	"github.com/nextlevelbuilder/goclaw/pkg/config"
	"github.com/nextlevelbuilder/goclaw/pkg/tracing"
)

// initOTelExporter is a no-op when built without the "otel" tag.
// Build with `go build -tags otel` to enable OpenTelemetry export.
func initOTelExporter(_ context.Context, _ *config.Config, _ *tracing.Collector) {
}
