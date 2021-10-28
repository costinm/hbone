module github.com/costinm/hbone/cmd/hbone-oc

go 1.16

replace github.com/costinm/hbone => ../..

require (
	contrib.go.opencensus.io/exporter/prometheus v0.4.0
	github.com/costinm/hbone v0.0.0-00010101000000-000000000000
	github.com/prometheus/client_golang v1.11.0
	go.opencensus.io v0.23.0
)
