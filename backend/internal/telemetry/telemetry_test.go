package telemetry

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	logs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
)

type testCollector struct {
	logs.UnimplementedLogsServiceServer
	requests chan *logs.ExportLogsServiceRequest
}

func (c *testCollector) Export(_ context.Context, request *logs.ExportLogsServiceRequest) (*logs.ExportLogsServiceResponse, error) {
	c.requests <- request
	return &logs.ExportLogsServiceResponse{}, nil
}

func TestNewLoggerKeepsLocalLoggingWhenOTLPIsNotConfigured(t *testing.T) {
	var output bytes.Buffer
	logger, shutdown, enabled, err := NewLogger(&output, func(string) string { return "" })
	if err != nil || enabled || shutdown != nil {
		t.Fatalf("unexpected disabled pipeline: enabled=%t shutdown=%v err=%v", enabled, shutdown != nil, err)
	}
	logger.Info("local logging works", "component", "test")
	if !strings.Contains(output.String(), "local logging works") {
		t.Fatalf("local output was not preserved: %q", output.String())
	}
}

func TestNewLoggerExportsToOTLPAndFlushesOnShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	grpcServer := grpc.NewServer()
	collector := &testCollector{requests: make(chan *logs.ExportLogsServiceRequest, 2)}
	logs.RegisterLogsServiceServer(grpcServer, collector)
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()

	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+listener.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	var local bytes.Buffer
	logger, shutdown, enabled, err := NewLogger(&local, func(name string) string {
		if name == "OTEL_EXPORTER_OTLP_ENDPOINT" {
			return "http://" + listener.Addr().String()
		}
		return ""
	})
	if err != nil || !enabled || shutdown == nil {
		t.Fatalf("OTLP pipeline did not initialize: enabled=%t shutdown=%v err=%v", enabled, shutdown != nil, err)
	}
	logger.Info("integration log", "component", "telemetry-test")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("OTLP shutdown/flush failed: %v", err)
	}
	if !strings.Contains(local.String(), "integration log") {
		t.Fatalf("local text handler did not receive the log: %q", local.String())
	}
	select {
	case request := <-collector.requests:
		if len(request.ResourceLogs) == 0 || len(request.ResourceLogs[0].ScopeLogs) == 0 || len(request.ResourceLogs[0].ScopeLogs[0].LogRecords) == 0 {
			t.Fatalf("collector received an empty log export: %+v", request)
		}
		var hasServiceName bool
		for _, attr := range request.ResourceLogs[0].Resource.Attributes {
			if attr.Key == "service.name" && attr.Value.GetStringValue() == ServiceName {
				hasServiceName = true
			}
		}
		if !hasServiceName {
			t.Fatalf("resource is missing service.name=%q: %+v", ServiceName, request.ResourceLogs[0].Resource)
		}
	default:
		t.Fatalf("OTLP collector received no records; local log was %q", local.String())
	}
}
