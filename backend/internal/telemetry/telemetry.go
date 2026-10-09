// Package telemetry configures optional OTLP log export for the Go server.
package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

const ServiceName = "ruview"

// NewLogger returns a local text logger and, when an OTLP endpoint is
// configured, a second handler that batches the same records to an OTLP/gRPC
// collector. Export setup errors leave local logging available and are
// returned without including exporter configuration or credentials.
func NewLogger(output io.Writer, getenv func(string) string) (*slog.Logger, func(context.Context) error, bool, error) {
	if output == nil {
		output = os.Stderr
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	local := slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT")) == "" && strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) == "" {
		return local, nil, false, nil
	}

	exporter, err := otlploggrpc.New(context.Background())
	if err != nil {
		return local, nil, false, errors.New("OTLP log exporter initialization failed")
	}
	processor := sdklog.NewBatchProcessor(
		exporter,
		sdklog.WithMaxQueueSize(512),
		sdklog.WithExportMaxBatchSize(128),
		sdklog.WithExportInterval(time.Second),
		sdklog.WithExportTimeout(5*time.Second),
	)
	provider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(processor),
		sdklog.WithResource(resource.NewWithAttributes("", attribute.String("service.name", ServiceName))),
	)
	remote := otelslog.NewHandler(ServiceName, otelslog.WithLoggerProvider(provider))
	combined := slog.New(fanoutHandler{handlers: []slog.Handler{local.Handler(), remote}})
	return combined, provider.Shutdown, true, nil
}

type fanoutHandler struct {
	handlers []slog.Handler
}

func (h fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h fanoutHandler) Handle(ctx context.Context, record slog.Record) error {
	var result error
	for _, handler := range h.handlers {
		if !handler.Enabled(ctx, record.Level) {
			continue
		}
		if err := handler.Handle(ctx, record.Clone()); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (h fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	result := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		result = append(result, handler.WithAttrs(attrs))
	}
	return fanoutHandler{handlers: result}
}

func (h fanoutHandler) WithGroup(name string) slog.Handler {
	result := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		result = append(result, handler.WithGroup(name))
	}
	return fanoutHandler{handlers: result}
}
