module github.com/costinm/hbone/ext/otel

go 1.16

replace github.com/costinm/hbone => ./../../..

require (
	github.com/costinm/hbone v0.0.0-20220924025508-eb8d3c95cef6
	github.com/prometheus/client_golang v1.13.0
	github.com/prometheus/client_model v0.2.0
	go.opentelemetry.io/contrib/instrumentation/host v0.36.1
	go.opentelemetry.io/contrib/instrumentation/runtime v0.36.1
	go.opentelemetry.io/contrib/zpages v0.36.1
	go.opentelemetry.io/otel v1.10.0
	go.opentelemetry.io/otel/exporters/stdout/stdoutmetric v0.32.1
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.10.0
	go.opentelemetry.io/otel/metric v0.32.1
	go.opentelemetry.io/otel/sdk v1.10.0
	go.opentelemetry.io/otel/sdk/metric v0.32.1
	go.opentelemetry.io/otel/trace v1.10.0
)
