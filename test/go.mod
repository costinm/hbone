module test

go 1.18

replace github.com/costinm/hbone => ../

replace github.com/costinm/hbone/urest => ../urest

replace github.com/costinm/hbone/ext/grpc => ../ext/grpc

replace github.com/costinm/ugate => ../../ugate

replace github.com/costinm/ugate/gen/proto => ../../ugate/gen/proto

replace github.com/costinm/ugate/auth => ../../ugate/auth

require (
	github.com/costinm/hbone/urest v0.0.0-00010101000000-000000000000
	github.com/costinm/ugate/gen/proto v0.0.0-00010101000000-000000000000
	github.com/costinm/ugate/auth v0.0.0-00010101000000-000000000000
	github.com/golang/protobuf v1.5.2
)

require (
	cloud.google.com/go v0.65.0 // indirect
	github.com/costinm/hbone v0.0.0-20220628165743-43be365c5ba8 // indirect
	golang.org/x/net v0.0.0-20211014172544-2b766c08f1c0 // indirect
	golang.org/x/oauth2 v0.0.0-20210819190943-2bc19b11175f // indirect
	golang.org/x/text v0.3.7 // indirect
	google.golang.org/appengine v1.6.6 // indirect
	google.golang.org/protobuf v1.28.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
