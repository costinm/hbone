module github.com/costinm/hbone/ext/urpc

go 1.18

replace github.com/costinm/hbone => ./..

require (
	github.com/costinm/hbone v0.0.0-00010101000000-000000000000
	github.com/costinm/meshauth v0.0.0-20221013185453-bb5aae6632f8
	github.com/golang/protobuf v1.5.2
	github.com/google/uuid v1.3.0
	google.golang.org/protobuf v1.28.1
)

require (
	github.com/google/go-cmp v0.5.6 // indirect
	golang.org/x/sys v0.0.0-20220926163933-8cfa568d3c25 // indirect
	golang.org/x/xerrors v0.0.0-20200804184101-5ec99f83aff1 // indirect
)
