module github.com/costinm/hbone/cmd/hbone-oc

go 1.16

replace github.com/costinm/hbone => ../..

require (
	github.com/costinm/hbone v0.0.0-00010101000000-000000000000
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.25.0
)
