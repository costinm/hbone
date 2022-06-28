module otel

go 1.16

require (
	github.com/costinm/hbone v0.0.0-20211105170253-a27d86dc30cf
	go.opentelemetry.io/contrib/instrumentation/host v0.26.1
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.26.1
	go.opentelemetry.io/contrib/instrumentation/runtime v0.26.1
	go.opentelemetry.io/otel v1.2.0
	go.opentelemetry.io/otel/exporters/stdout/stdoutmetric v0.25.0
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.2.0
	go.opentelemetry.io/otel/metric v0.25.0
	go.opentelemetry.io/otel/sdk v1.2.0
	go.opentelemetry.io/otel/sdk/metric v0.25.0
	sigs.k8s.io/controller-runtime v0.10.3
)
