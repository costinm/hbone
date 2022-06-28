package otel

import (
	"context"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/costinm/hbone"

	"go.opentelemetry.io/otel"

	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"

	"go.opentelemetry.io/contrib/instrumentation/host"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"

	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"

	"go.opentelemetry.io/otel/propagation"

	"go.opentelemetry.io/otel/metric/global"
	metricsController "go.opentelemetry.io/otel/sdk/metric/controller/basic"
	metricsProcessor "go.opentelemetry.io/otel/sdk/metric/processor/basic"
	"go.opentelemetry.io/otel/sdk/metric/selector/simple"

	semconv "go.opentelemetry.io/otel/semconv/v1.7.0"
)

// Integration with Otel APIs. Includes the stdout handlers for debug
// and local execution. Stackdriver or otel gRPC can be used in prod.

// This is part of hbone package since 'request' metrics should be collected
// here - the package includes basic utilities for mesh networking, which includes
// certs and metric collection..

// TODO: pass a 'Config' interface (k8s/mesh-env)
// Reconfiguration, telemetry or otel API

func OTelEnable(hb *hbone.HBone) {
	hb.Transport = func(t http.RoundTripper) http.RoundTripper {
		return otelhttp.NewTransport(t)
	}
	hb.HandlerWrapper = func(h http.Handler) http.Handler {
		return otelhttp.NewHandler(h, "/hbone")
	}
	// Host telemetry -
	host.Start()

	if err := runtime.Start(
		runtime.WithMinimumReadMemStatsInterval(time.Second),
	); err != nil {
		log.Fatalln("failed to start runtime instrumentation:", err)
	}
}


// FileExporter configures a file exporter for metrics, traces, logs
func FileExporter(ctx context.Context, w io.Writer) func() {

	// Creates an SpanExporter object
	// exp.shutdown is expected to be called before exit.
	spanExporter, err := stdouttrace.New(
		stdouttrace.WithWriter(w),
		// Use human readable output.
		stdouttrace.WithPrettyPrint(),
		//stdouttrace.WithoutTimestamps(),
	)
	if err != nil {
		log.Fatal(err)
	}

	tp := trace.NewTracerProvider(
		trace.WithBatcher(spanExporter),
		trace.WithSampler(trace.AlwaysSample()),
		trace.WithResource(resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceNameKey.String("ExampleService"))),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	cleanup := FileMetricsExporter(ctx, w)

	// End telemetry magic
	return func() {
		cleanup()
		if err := tp.Shutdown(context.Background()); err != nil {
			log.Fatal(err)
		}
	}

}

func FileMetricsExporter(ctx context.Context, w io.Writer) func() {
	exporter, err := stdoutmetric.New(
		//stdoutmetric.WithPrettyPrint(),
		stdoutmetric.WithWriter(w))
	if err != nil {
		log.Fatalf("creating stdoutmetric exporter: %v", err)
	}

	pusher := metricsController.New(
		metricsProcessor.NewFactory(
			simple.NewWithInexpensiveDistribution(),
			exporter,
			metricsProcessor.WithMemory(true),
		),
		metricsController.WithExporter(exporter),
		metricsController.WithCollectPeriod(3*time.Second),
		// WithResource, WithCollectPeriod, WithPushTimeout
	)

	if err = pusher.Start(ctx); err != nil {
		log.Fatalf("starting push controller: %v", err)
	}

	global.SetMeterProvider(pusher)


	return func() {
		if err := pusher.Stop(ctx); err != nil {
			log.Fatalf("stopping push controller: %v", err)
		}
	}
}
