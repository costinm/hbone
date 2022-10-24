module github.com/costinm/hbone/ext/otel

go 1.18

replace github.com/costinm/hbone => ../..

require (
	github.com/VictoriaMetrics/metrics v1.22.2
	github.com/costinm/hbone v0.0.0-20220924025508-eb8d3c95cef6
)

require (
	github.com/valyala/fastrand v1.1.0 // indirect
	github.com/valyala/histogram v1.2.0 // indirect
	golang.org/x/sys v0.0.0-20220926163933-8cfa568d3c25 // indirect
)
