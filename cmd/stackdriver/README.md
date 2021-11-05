Small CLI tool to verify metrics exported to stackdriver, primarily for Istio.

Istio supported metrics:
  https://cloud.google.com/monitoring/api/metrics_istio




WIP.

Integration and testing with stackdriver for 'proxyless' modes.

With Envoy, this is implemented using WASM or native filters.

For proxyless (gRPC or generic hbone / uProxy) we need to:
- decode and generate the Istio header containing client info
- generate the expected istio metrics. 


It is also possible to use the REST API:
monitoring.googleapis.com/v3/projects/NAME/timeSeries
  ?aggregation.alignmentPeriod=60s
  &aggregation.crossSeriesReducer=REDUCE_NONE
  &aggregation.perSeriesAligner=ALIGN_RATE
  &alt=json
  &filter=metric.type+%3D+%22istio.io%2Fservice%2Fclient%2Frequest_count%22+AND+resource.type+%3D+%22istio_canonical_service%22+AND+resource.labels.namespace_name+%3D+%22fortio%22
 &interval.endTime=2021-09-30T14%3A32%3A51-07%3A00
 &interval.startTime=2021-09-30T14%3A27%3A51-07%3A00
 &prettyPrint=false

Typical metric labels:

destination_canonical_revision:latest
destination_canonical_service_name:fortio-cr
destination_canonical_service_namespace:fortio
destination_owner:unknown
destination_port:15442
destination_principal:spiffe://wlhe-cr.svc.id.goog/ns/fortio/sa/default
destination_service_name:fortio-cr-icq63pqnqq-uc
destination_service_namespace:fortio
destination_workload_name:fortio-cr-sni
destination_workload_namespace:fortio
mesh_uid:proj-601426346923
request_operation:GET
request_protocol:http
response_code:200
service_authentication_policy:unknown
source_canonical_revision:v1
source_canonical_service_name:fortio
source_canonical_service_namespace:fortio
source_owner:kubernetes://apis/apps/v1/namespaces/fortio/deployments/fortio
source_principal:spiffe://wlhe-cr.svc.id.goog/ns/fortio/sa/default
source_workload_name:fortio
source_workload_namespace:fortio


