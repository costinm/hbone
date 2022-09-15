module github.com/costinm/hbone/ext/uxds

go 1.18

replace github.com/costinm/hbone => ../..

replace github.com/costinm/hbone/ext/http2 => ../../ext/http2

replace github.com/costinm/hbone/ext/transport => ../../ext/transport

require (
	github.com/costinm/hbone v0.0.0-00010101000000-000000000000
	github.com/golang/protobuf v1.5.2
	google.golang.org/protobuf v1.28.1
)

require (
	github.com/costinm/hbone/ext/http2 v0.0.0-00010101000000-000000000000 // indirect
	github.com/costinm/hbone/ext/transport v0.0.0-00010101000000-000000000000 // indirect
	golang.org/x/net v0.0.0-20220812174116-3211cb980234 // indirect
	golang.org/x/sys v0.0.0-20220728004956-3c1f35247d10 // indirect
	golang.org/x/text v0.3.7 // indirect
	google.golang.org/genproto v0.0.0-20220707144311-dc4cdde2ef63 // indirect
	google.golang.org/grpc v1.47.0 // indirect
)
