module github.com/costinm/hbone

go 1.16

replace github.com/costinm/hbone/ext/http2 => ./ext/http2/

replace github.com/costinm/hbone => ./

replace github.com/costinm/hbone/ext/transport => ./ext/transport/

require (
	github.com/costinm/hbone/ext/http2 v0.0.0-00010101000000-000000000000
	github.com/costinm/hbone/ext/transport v0.0.0-20220802025232-e3a8f640f9f6 // indirect
	sigs.k8s.io/yaml v1.3.0
)
