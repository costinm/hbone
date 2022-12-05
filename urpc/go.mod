module github.com/costinm/hbone/urpc

go 1.18

replace github.com/costinm/hbone => ./..

//replace github.com/costinm/meshauth => ../../meshauth

require (
	github.com/costinm/hbone v0.0.0-00010101000000-000000000000
	github.com/costinm/meshauth v0.0.0-20221013185453-bb5aae6632f8
	github.com/golang/protobuf v1.5.2
	github.com/google/uuid v1.3.0
	github.com/hashicorp/go-multierror v1.1.1
	golang.org/x/sync v0.1.0
	google.golang.org/grpc v1.50.1
	google.golang.org/protobuf v1.28.1
	sigs.k8s.io/yaml v1.3.0
)

require (
	github.com/hashicorp/errwrap v1.0.0 // indirect
	golang.org/x/net v0.1.0 // indirect
	golang.org/x/sys v0.1.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
)
