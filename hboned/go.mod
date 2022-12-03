module github.com/costinm/hbone/hboned

go 1.18

replace github.com/costinm/hbone => ./..

replace github.com/costinm/meshauth => ./../../meshauth

replace github.com/costinm/hbone/urpc => ../urpc

//replace github.com/costinm/hbone/ext/tel => ./../ext/tel

require (
	github.com/costinm/hbone v0.0.0-20221024011056-f21d675d788b
	github.com/costinm/hbone/urpc v0.0.0-20221024011056-f21d675d788b
	github.com/costinm/meshauth v0.0.0-20221024010349-600f57eab6c7
	github.com/golang/protobuf v1.5.2
	golang.org/x/oauth2 v0.1.0
	sigs.k8s.io/yaml v1.3.0
)

require (
	cloud.google.com/go/compute v1.10.0 // indirect
	github.com/google/go-cmp v0.5.9 // indirect
	github.com/google/uuid v1.3.0 // indirect
	github.com/kr/pretty v0.1.0 // indirect
	golang.org/x/net v0.1.0 // indirect
	golang.org/x/sys v0.1.0 // indirect
	google.golang.org/appengine v1.6.7 // indirect
	google.golang.org/protobuf v1.28.1 // indirect
	gopkg.in/check.v1 v1.0.0-20190902080502-41f04d3bba15 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
