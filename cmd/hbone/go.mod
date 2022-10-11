module github.com/costinm/hbone/cmd/hbone

go 1.16

replace github.com/costinm/hbone => ../..

replace github.com/costinm/hbone/ext/gcp => ../../ext/gcp

replace github.com/costinm/hbone/ext/uxds => ../../ext/uxds

replace github.com/costinm/hbone/ext/otel => ../../ext/otel

require (
	github.com/costinm/hbone v0.0.0-20220802025232-e3a8f640f9f6
	github.com/costinm/hbone/ext/gcp v0.0.0-20220802025232-e3a8f640f9f6
	github.com/costinm/hbone/ext/otel v0.0.0-20220915032909-016282120b33
	github.com/costinm/hbone/ext/uxds v0.0.0-00010101000000-000000000000
	sigs.k8s.io/yaml v1.3.0
)
