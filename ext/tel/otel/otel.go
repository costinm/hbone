package otel

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/host"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/contrib/zpages"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/metric/global"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"

	semconv "go.opentelemetry.io/otel/semconv/v1.7.0"
)

// FileExporter configures a file exporter for metrics, traces, logs
func FileExporter(ctx context.Context, w io.Writer) func() {
	res := resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceNameKey.String("ExampleService"))

	cleanup := FileMetricsExporter(context.Background(), w, res)

	return cleanup
}

func FileExporterT(ctx context.Context, w io.Writer, sn string) func() {
	// service.name = canonical service
	res := resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceNameKey.String(sn))

	// Creates an SpanExporter object
	// exp.shutdown is expected to be called before exit.
	spanExporter, err := stdouttrace.New(
		stdouttrace.WithWriter(w),
		// Use human readable output.
		//stdouttrace.WithPrettyPrint(),
		//stdouttrace.WithoutTimestamps(),
	)
	if err != nil {
		log.Fatal(err)
	}

	sp := zpages.NewSpanProcessor()
	thandler := zpages.NewTracezHandler(sp)
	http.DefaultServeMux.Handle("/debug/tracez", thandler)

	tp := trace.NewTracerProvider(
		trace.WithBatcher(spanExporter),
		trace.WithSampler(trace.AlwaysSample()),
		trace.WithResource(res),
		trace.WithSpanProcessor(sp),
	)

	otel.SetTracerProvider(tp)

	// Info to propagate between nodes via Baggage
	otel.SetTextMapPropagator(
		propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{}))

	cleanup := FileMetricsExporter(ctx, w, res)

	// End telemetry magic
	return func() {
		cleanup()
		if err := tp.Shutdown(context.Background()); err != nil {
			log.Fatal(err)
		}
	}
}

func FileMetricsExporter(ctx context.Context, w io.Writer, res *resource.Resource) func() {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	exporter, err := stdoutmetric.New(
		stdoutmetric.WithEncoder(enc))
	if err != nil {
		log.Fatalf("creating stdoutmetric exporter: %v", err)
	}

	// Push the metrics with this interval.
	meterProvider := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(exporter,
			metric.WithInterval(1*time.Second))),
	)

	global.SetMeterProvider(meterProvider)

	return func() {
		exporter.ForceFlush(ctx)
		_ = meterProvider.Shutdown(ctx)
		exporter.Shutdown(ctx)
	}
}

// Should be called only if another main function didn't already initialize this.
func InitOTelHost() {
	// Host telemetry -
	host.Start()

	if err := runtime.Start(
		runtime.WithMinimumReadMemStatsInterval(time.Second),
	); err != nil {
		log.Fatalln("failed to start runtime instrumentation:", err)
	}
}
