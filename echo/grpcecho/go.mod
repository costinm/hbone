module github.com/costinm/hbone/echo/grpcecho

go 1.16

replace github.com/costinm/hbone => ../..

replace github.com/costinm/hbone/ext/http2 => ../../ext/http2

require google.golang.org/protobuf v1.28.1

require (
	github.com/coreos/pkg v0.0.0-20220810130054-c7d1c02cb6cf
	github.com/costinm/hbone v0.0.0-00010101000000-000000000000
	github.com/google/go-cmp v0.5.7 // indirect
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c
	google.golang.org/grpc v1.49.0
)
