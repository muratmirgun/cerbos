// Copyright 2021-2023 Zenauth Ltd.
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"fmt"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"net"
	"net/http"
	"strings"

	octrace "go.opencensus.io/trace"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/contrib/propagators/autoprop"
	otelpropb3 "go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel"
	ocbridge "go.opentelemetry.io/otel/bridge/opencensus"
	"go.opentelemetry.io/otel/exporters/jaeger" //nolint:staticcheck
	otlp "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otlphttp "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprop "go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.18.0"
	"go.opentelemetry.io/otel/semconv/v1.18.0/httpconv"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cerbos/cerbos/internal/config"
	"github.com/cerbos/cerbos/internal/util"
)

var conf Conf

func Init(ctx context.Context) error {
	if err := config.GetSection(&conf); err != nil {
		return fmt.Errorf("failed to load tracing config: %w", err)
	}

	return InitFromConf(ctx, conf)
}

func InitFromConf(ctx context.Context, conf Conf) error {
	switch conf.Exporter {
	case jaegerExporter:
		return configureJaeger(ctx)
	case otlpExporter:
		return configureOTLP(ctx)
	case "":
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
		return nil
	default:
		return fmt.Errorf("unknown exporter %q", conf.Exporter)
	}
}

func configureJaeger(ctx context.Context) error {
	var endpoint jaeger.EndpointOption
	if conf.Jaeger.AgentEndpoint != "" {
		agentHost, agentPort, err := net.SplitHostPort(conf.Jaeger.AgentEndpoint)
		if err != nil {
			return fmt.Errorf("failed to parse agent endpoint %q: %w", conf.Jaeger.AgentEndpoint, err)
		}

		endpoint = jaeger.WithAgentEndpoint(jaeger.WithAgentHost(agentHost), jaeger.WithAgentPort(agentPort))
	} else {
		endpoint = jaeger.WithCollectorEndpoint(jaeger.WithEndpoint(conf.Jaeger.CollectorEndpoint))
	}

	exporter, err := jaeger.New(endpoint)
	if err != nil {
		return fmt.Errorf("failed to create Jaeger exporter: %w", err)
	}

	svcName := conf.ServiceName
	if svcName == nil {
		svcName = &conf.Jaeger.ServiceName
	}

	return configureOtel(ctx, svcName, exporter)
}

func configureOTLP(ctx context.Context) error {
	var exporter *otlptrace.Exporter
	var err error

	switch conf.OTLP.Protocol {
	case "grpc":
		conn, err := grpc.DialContext(ctx, conf.OTLP.CollectorEndpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return fmt.Errorf("failed to dial otlp collector: %w", err)
		}

		exporter, err = otlp.New(ctx, otlp.WithGRPCConn(conn))
		if err != nil {
			return fmt.Errorf("failed to create otlp exporter: %w", err)
		}
	case "http":
		exporter, err = otlphttp.New(ctx, otlphttp.WithEndpoint(conf.OTLP.CollectorEndpoint))
		if err != nil {
			return fmt.Errorf("failed to create otlp exporter: %w", err)
		}
	default:
		return fmt.Errorf("unknown OTLP protocol %q. Supported protocols are 'grpc' and 'http'", conf.OTLP.Protocol)
	}

	return configureOtel(ctx, conf.ServiceName, exporter)
}

func configureOtel(ctx context.Context, svcName *string, exporter tracesdk.SpanExporter) error {
	sampler := mkSampler(conf.SampleProbability)

	if svcName == nil {
		svcName = &util.AppName
	}

	res, err := resource.New(context.Background(),
		resource.WithAttributes(semconv.ServiceNameKey.String(*svcName)),
		resource.WithProcessPID(),
		resource.WithHost(),
		resource.WithFromEnv())
	if err != nil {
		return fmt.Errorf("failed to initialize otel resource: %w", err)
	}

	traceProvider := tracesdk.NewTracerProvider(
		tracesdk.WithBatcher(exporter),
		tracesdk.WithSampler(sampler),
		tracesdk.WithResource(res),
	)

	otel.SetErrorHandler(otelErrHandler(func(err error) {
		// this is a harmless error message that occurs because Otel doesn't recognise
		// the OpenCensus sampler. We can remove this check when OpenCensus is replaced.
		if strings.Contains(err.Error(), "unsupported sampler:") {
			return
		}

		zap.L().Named("otel").Warn("OpenTelemetry error", zap.Error(err))
	}))

	otel.SetTracerProvider(traceProvider)
	otel.SetTextMapPropagator(autoprop.NewTextMapPropagator(otelprop.TraceContext{}, otelprop.Baggage{}, otelpropb3.New()))
	octrace.DefaultTracer = ocbridge.NewTracer(traceProvider.Tracer("cerbos"))

	go func() {
		<-ctx.Done()
		// TODO (cell) Add hook to make the server wait until the trace provider shuts down cleanly.

		if err := traceProvider.Shutdown(context.TODO()); err != nil {
			zap.L().Warn("Failed to cleanly shutdown trace exporter", zap.Error(err))
		}
	}()

	return nil
}

func mkSampler(probability float64) tracesdk.Sampler {
	if probability == 0.0 {
		return tracesdk.NeverSample()
	}

	return sampler{s: tracesdk.ParentBased(tracesdk.TraceIDRatioBased(conf.SampleProbability))}
}

type sampler struct {
	s tracesdk.Sampler
}

func (s sampler) ShouldSample(params tracesdk.SamplingParameters) tracesdk.SamplingResult {
	switch {
	case strings.HasPrefix(params.Name, "grpc."):
		return tracesdk.SamplingResult{Decision: tracesdk.Drop}
	case strings.HasPrefix(params.Name, "cerbos.svc.v1.CerbosPlaygroundService."):
		return tracesdk.SamplingResult{Decision: tracesdk.Drop}
	case strings.HasPrefix(params.Name, "/api/playground/"):
		return tracesdk.SamplingResult{Decision: tracesdk.Drop}
	default:
		return s.s.ShouldSample(params)
	}
}

func (s sampler) Description() string {
	return "CerbosCustomSampler"
}

func HTTPHandler(handler http.Handler, path string) http.Handler {
	return otelhttp.NewHandler(handler, path)
}

func StartSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	return otel.Tracer("cerbos.dev/cerbos").Start(ctx, name)
}

func MarkFailed(span trace.Span, code int, err error) {
	if err != nil {
		span.RecordError(err)
	}

	c, desc := httpconv.ServerStatus(code)
	span.SetStatus(c, desc)
}

type otelErrHandler func(err error)

func (o otelErrHandler) Handle(err error) {
	o(err)
}
