module test

go 1.18

replace github.com/costinm/hbone => ../

replace github.com/costinm/hbone/ext/grpc => ../ext/grpc

replace github.com/costinm/hbone/ext/gcp => ../ext/gcp

replace github.com/costinm/ugate/gen/proto => ../../ugate/gen/proto

replace github.com/costinm/ugate/auth => ../../ugate/auth

require (
	github.com/costinm/hbone v0.0.0-20220628165743-43be365c5ba8
	github.com/costinm/hbone/ext/gcp v0.0.0-00010101000000-000000000000
	github.com/costinm/hbone/ext/grpc v0.0.0-00010101000000-000000000000
	github.com/costinm/ugate/auth v0.0.0-00010101000000-000000000000
	github.com/costinm/ugate/gen/proto v0.0.0-00010101000000-000000000000
	google.golang.org/protobuf v1.28.0
)

require (
	github.com/golang/protobuf v1.5.2 // indirect
	golang.org/x/net v0.0.0-20220624214902-1bab6f366d9e // indirect
	golang.org/x/oauth2 v0.0.0-20220630143837-2104d58473e0 // indirect
	golang.org/x/text v0.3.7 // indirect
	google.golang.org/appengine v1.6.7 // indirect
)
