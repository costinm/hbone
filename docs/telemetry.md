# Expvar

Goals: 
- Represent prometheus/otel metrics and labels using Expvar interface, with minimal dependencies.
- Export using prom text format - no deps
- Maybe: export in otel/ometrics proto format
- Access to metric values from code and tests.

How: 
- handler to export expvar in metrics format
- use prom naming scheme for expvar metrics
- labels as Map values

# Getting the stats

```
kubectl -n fortio exec fortio-mcp-68cf88799f-qjfh5 -- curl localhost:15020/stats/prometheus
```

Example prom:

```text

istio_requests_total{response_code="200",reporter="destination",source_workload="unknown",source_workload_namespace="fortio",source_principal="spiffe://wlhe-cr.svc.id.goog/ns/fortio/sa/default",source_app="fortio-cr",source_version="v1",source_cluster="cn-wlhe-cr-us-central1-c-istio",destination_workload="fortio-mcp",destination_workload_namespace="fortio",destination_principal="spiffe://wlhe-cr.svc.id.goog/ns/fortio/sa/default",destination_app="fortio-mcp",destination_version="v1",destination_service="fortio-mcp.fortio.svc.cluster.local",destination_service_name="fortio-mcp",destination_service_namespace="fortio",destination_cluster="cn-wlhe-cr-us-central1-c-istio",request_protocol="http",response_flags="-",grpc_response_status="",connection_security_policy="mutual_tls",source_canonical_service="fortio-cr",destination_canonical_service="fortio-mcp",source_canonical_revision="latest",destination_canonical_revision="v1"} 895201

istio_request_bytes_bucket{response_code="200",reporter="destination",source_workload="unknown",source_workload_namespace="fortio",source_principal="spiffe://wlhe-cr.svc.id.goog/ns/fortio/sa/default",source_app="fortio-cr",source_version="v1",source_cluster="cn-wlhe-cr-us-central1-c-istio",destination_workload="fortio-mcp",destination_workload_namespace="fortio",destination_principal="spiffe://wlhe-cr.svc.id.goog/ns/fortio/sa/default",destination_app="fortio-mcp",destination_version="v1",destination_service="fortio-mcp.fortio.svc.cluster.local",destination_service_name="fortio-mcp",destination_service_namespace="fortio",destination_cluster="cn-wlhe-cr-us-central1-c-istio",request_protocol="http",response_flags="-",grpc_response_status="",connection_security_policy="mutual_tls",source_canonical_service="fortio-cr",destination_canonical_service="fortio-mcp",source_canonical_revision="latest",destination_canonical_revision="v1",le="5000"} 895201

istio_request_duration_milliseconds_bucket{response_code="503",reporter="source",source_workload="fortio-mcp",source_workload_namespace="fortio",source_principal="spiffe://wlhe-cr.svc.id.goog/ns/fortio/sa/default",source_app="fortio-mcp",source_version="v1",source_cluster="cn-wlhe-cr-us-central1-c-istio",destination_workload="fortio-cr-sni",destination_workload_namespace="fortio",destination_principal="spiffe://wlhe-cr.svc.id.goog/ns/fortio/sa/default",destination_app="unknown",destination_version="unknown",destination_service="fortio-cr-icq63pqnqq-uc.fortio.svc.cluster.local",destination_service_name="fortio-cr-icq63pqnqq-uc",destination_service_namespace="fortio",destination_cluster="unknown",request_protocol="http",response_flags="UC",grpc_response_status="",connection_security_policy="unknown",source_canonical_service="fortio-mcp",destination_canonical_service="fortio-cr",source_canonical_revision="v1",destination_canonical_revision="latest",le="30000"} 5


```

# Open census

Proxyless gRPC is using OC library - to avoid extra size we will use the same, with a forked plugin.


Last version: 0.23 

- No interface and lots of dependencies
  - grpc 1.33.2 -> protobuf 1.4.1
  - grpc also depends on go-control-plane, which depends on grpc 1.25.1 and protobuf 1.3.2
  - in total 5 versions of grpc linked from the gprc 1.33.2, including 1.19 from 2018

Metrics are created with 'stats.Int64(name, desc, unit)'

Metrics are added with 'stats.Record()', and tags are part of the context.

Model:
- "measurement" is a data point - latency, one request, etc, with labels
- "measure" has name, description, unit, type (int64 or float64) - and is associated with a list of measurements. The "M" method 
 creates a new measurement, and stats.Record() adds it to the database.
- "view" couple "measure" with aggregation and tags. Also has a name, description, measure, aggreagation function and the list of tags.

# Open Telemetry

The library is still very heavy - why does it need go-logr and yaml deps ? 


- library uses interfaces, individual go modules
- 'contrib' has detectors - for example get region, etc. For GKE uses env NAMESPACE, HOSTNAME, CONTAINER_NAME - and "cluster_name" metadata.
- 'contrib' instrumentations - http, etc.
- dictionary of attributes, including for 'resources' (GCP, pod, etc) 'semantic conventions'

- context holds the span and attributes

```go

```

Model:
- MetricProvider - config, impl
- Meter - instrumentation library. Create Instruments.
- Instrument (type, name, attributes). Counter, Histogram, Gauge, Create Measurement
- Measurement - value and timestamp, AND attributes. Similar with an event.
- Event based - each measurement is an event
- Context is used by sync instruments, callback for async
- attributes set at Measurement time - Instrument.Add(1, attribute...)

The "View" controls how measurement are processed and sent. View can also add 
attributes from context. This is independent of the generation of Measurement.


# Prometheus

The client library for prom in go is one of the most convoluted and unnecesarily complex
code I've seen ( or written :-). The prom server doesn't use or care about descriptions
and all the channels are over complex.

VictoriaMetrics is a much simpler implementation, with few external deps. Still a bit 
too much. Has a good historgram and summary impl as well.

github.com/prometheus/client_golang/prometheus

- 'registry' typically created in init
- add collectors with "MostRegister" - example GoCollector, ProcessCollector.
- Counter.build().name(name).help(help).register()
- Counter.inc(double), Gauge.inc/dec/set/get
- Histogram - durations, sizes - in buckets. Includes sum of all observed values.
- Summary - total sum, count, quantiles.

```go
rpcDurations = prometheus.NewSummaryVec(
prometheus.SummaryOpts{
Name:       "rpc_durations_seconds",
Help:       "RPC latency distributions.",
Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
},
[]string{"service"},
)
rpcDurations.WithLabelValues("uniform").Observe(v)

```

Example: https://github.com/prometheus/haproxy_exporter/blob/master/haproxy_exporter.go

Model:
- time series - a stream of timestamped values associated with a 'metric' and set of labeled dimensions. The ID is based on name and the label key+value
- sample - float64+timestamp, part of a time series.

Notation from OpenTSDB: api_http_requests_total{method="POST", handler="/messages"}

Dependencies: protobuf 1.4.3 and few others.

# K8S client

```go

// DurationMetric is a measurement of some amount of time.
type DurationMetric interface {
	Observe(duration time.Duration)
}

// LatencyMetric observes client latency partitioned by verb and url.
type LatencyMetric interface {
	Observe(ctx context.Context, verb string, u url.URL, latency time.Duration)
}

// ResultMetric counts response codes partitioned by method and host.
type ResultMetric interface {
	Increment(ctx context.Context, code string, method string, host string)
}


// RegisterOpts contains all the metrics to register. Metrics may be nil.
type RegisterOpts struct {
RequestLatency LatencyMetric
RequestResult  ResultMetric
}

var registerMetrics sync.Once

// Register registers metrics for the rest client to use. This can
// only be called once.
func Register(opts RegisterOpts) {
registerMetrics.Do(func() {
if opts.RequestLatency != nil {
RequestLatency = opts.RequestLatency
}
if opts.RequestResult != nil {
RequestResult = opts.RequestResult
}
})
}
```

