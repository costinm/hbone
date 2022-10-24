module github.com/costinm/hbone/ext/otel

go 1.16

replace github.com/costinm/hbone => ./../../..

require (
	github.com/costinm/hbone v0.0.0-20220924025508-eb8d3c95cef6
	github.com/prometheus/client_golang v1.13.0
	github.com/prometheus/client_model v0.2.0
)
