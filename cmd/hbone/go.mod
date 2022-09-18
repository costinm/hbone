module github.com/costinm/hbone/cmd/hbone

go 1.16

replace github.com/costinm/hbone => ../..

replace github.com/costinm/hbone/ext/gcp => ../../ext/gcp

replace github.com/costinm/hbone/ext/uxds => ../../ext/uxds

require (
	github.com/costinm/hbone v0.0.0-20220802025232-e3a8f640f9f6
	github.com/costinm/hbone/ext/gcp v0.0.0-20220802025232-e3a8f640f9f6
	github.com/costinm/hbone/ext/uxds v0.0.0-00010101000000-000000000000
	gopkg.in/check.v1 v1.0.0-20190902080502-41f04d3bba15 // indirect
	sigs.k8s.io/yaml v1.3.0
)
