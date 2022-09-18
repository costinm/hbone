module test

go 1.18

replace github.com/costinm/hbone => ../

replace github.com/costinm/hbone/ext/gcp => ../ext/gcp

replace github.com/costinm/hbone/ext/uxds => ../ext/uxds

replace github.com/costinm/ugate/gen/proto => ../../ugate/gen/proto

replace github.com/costinm/ugate/auth => ../../ugate/auth

require (
	github.com/costinm/hbone v0.0.0-20220915032909-016282120b33
	github.com/costinm/hbone/ext/gcp v0.0.0-00010101000000-000000000000
	github.com/costinm/hbone/ext/uxds v0.0.0-00010101000000-000000000000
	github.com/golang/protobuf v1.5.2
)

require (
	cloud.google.com/go/compute v1.7.0 // indirect
	golang.org/x/net v0.0.0-20220812174116-3211cb980234 // indirect
	golang.org/x/oauth2 v0.0.0-20220630143837-2104d58473e0 // indirect
	golang.org/x/sys v0.0.0-20220915200043-7b5979e65e41 // indirect
	golang.org/x/text v0.3.7 // indirect
	google.golang.org/protobuf v1.28.1 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	sigs.k8s.io/yaml v1.3.0 // indirect
)
