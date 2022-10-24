module github.com/costinm/hbone/hboned

go 1.18

replace github.com/costinm/hbone => ./..

replace github.com/costinm/meshauth => ./../../meshauth

replace github.com/costinm/hbone/urpc => ../urpc

replace github.com/costinm/hbone/ext/tel => ./../ext/tel

require (
	github.com/costinm/hbone v0.0.0-20220924025508-eb8d3c95cef6
	github.com/costinm/hbone/ext/tel v0.0.0-00010101000000-000000000000
	github.com/costinm/meshauth v0.0.0-20221013185453-bb5aae6632f8
	github.com/golang/protobuf v1.5.2
	golang.org/x/oauth2 v0.0.0-20220909003341-f21342109be1
	sigs.k8s.io/yaml v1.3.0
)

require (
	cloud.google.com/go/compute v1.10.0 // indirect
	github.com/VictoriaMetrics/metrics v1.22.2 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.1.2 // indirect
	github.com/google/go-cmp v0.5.9 // indirect
	github.com/google/uuid v1.3.0 // indirect
	github.com/matttproud/golang_protobuf_extensions v1.0.2 // indirect
	github.com/prometheus/client_golang v1.13.0 // indirect
	github.com/prometheus/client_model v0.2.0 // indirect
	github.com/prometheus/common v0.37.0 // indirect
	github.com/prometheus/procfs v0.8.0 // indirect
	github.com/valyala/fastrand v1.1.0 // indirect
	github.com/valyala/histogram v1.2.0 // indirect
	golang.org/x/net v0.0.0-20220923203811-8be639271d50 // indirect
	golang.org/x/sys v0.0.0-20220926163933-8cfa568d3c25 // indirect
	google.golang.org/appengine v1.6.7 // indirect
	google.golang.org/protobuf v1.28.1 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
