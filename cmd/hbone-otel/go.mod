module github.com/costinm/hbone/cmd/hbone-otel

go 1.16

replace github.com/costinm/hbone => ../..

replace github.com/costinm/hbone/ext/otel => ../../ext/otel

require (
	github.com/costinm/hbone v0.0.0-20211105170253-a27d86dc30cf
	github.com/costinm/hbone/ext/otel v0.0.0-00010101000000-000000000000
)
