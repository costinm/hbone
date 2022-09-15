module github.com/costinm/hbone/ext/gcp

go 1.18

replace github.com/costinm/hbone => ../..

replace github.com/costinm/hbone/ext/http2 => ../../ext/http2

require (
	github.com/costinm/hbone v0.0.0-20220628165743-43be365c5ba8
	golang.org/x/oauth2 v0.0.0-20220630143837-2104d58473e0
)

require (
	cloud.google.com/go/compute v1.7.0 // indirect
	github.com/costinm/hbone/ext/http2 v0.0.0-00010101000000-000000000000 // indirect
	github.com/golang/protobuf v1.5.2 // indirect
	golang.org/x/net v0.0.0-20220812174116-3211cb980234 // indirect
	golang.org/x/text v0.3.7 // indirect
	google.golang.org/appengine v1.6.7 // indirect
	google.golang.org/protobuf v1.28.0 // indirect
)
