// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"google.golang.org/api/monitoring/v3"
)

var (
	p = flag.String("p", os.Getenv("PROJECT_ID"), "Project ID")
	r = flag.String("r", "", "Resource type")
	ns = flag.String("n", "", "Namespace")
	metric = flag.String("m", "istio.io/service/client/request_count", "Metric name")
	extra = flag.String("x", "", "Extra query parameters")
)

func main() {
	flag.Parse()
	projectID := *p
	if *p == "" {
		projectID = "dmeshgate"
		//panic("Missing PROJECT_ID")
		//return
	}

	sd, err := NewStackdriver(projectID)
	if err != nil {
		panic(err)
	}
	// Typical metric:
	// destination_canonical_revision:latest
	// destination_canonical_service_name:fortio-cr
	// destination_canonical_service_namespace:fortio
	// destination_owner:unknown
	// destination_port:15442
	// destination_principal:spiffe://wlhe-cr.svc.id.goog/ns/fortio/sa/default
	// destination_service_name:fortio-cr-icq63pqnqq-uc
	// destination_service_namespace:fortio
	// destination_workload_name:fortio-cr-sni
	// destination_workload_namespace:fortio
	// mesh_uid:proj-601426346923
	// request_operation:GET
	// request_protocol:http
	// response_code:200
	// service_authentication_policy:unknown
	// source_canonical_revision:v1
	// source_canonical_service_name:fortio
	// source_canonical_service_namespace:fortio
	// source_owner:kubernetes://apis/apps/v1/namespaces/fortio/deployments/fortio
	// source_principal:spiffe://wlhe-cr.svc.id.goog/ns/fortio/sa/default
	// source_workload_name:fortio
	// source_workload_namespace:fortio

	// TODO: retry, set a limit on how big of a delay is ok

	// Verify client side metrics (in pod) reflect the CloudrRun server properties
	ts, err := sd.ListTimeSeries(context.Background(),
		*ns, *r,
		*metric, *extra)
		//" AND metric.labels.source_canonical_service_name = \"fortio\"" +
		//		" AND metric.labels.response_code = \"200\"")
	if err != nil {
		log.Fatalf("Error %v", err)
	}

	for _, tsr := range ts {
		log.Println(tsr.Metric.Labels)
		v := tsr.Points[0].Value
		log.Println(*v.DoubleValue)

	}
}


// WIP.
//
// Integration and testing with stackdriver for 'proxyless' modes.
//
// With Envoy, this is implemented using WASM or native filters.
//
// For proxyless (gRPC or generic hbone / uProxy) we need to:
// - decode and generate the Istio header containing client info
// - generate the expected istio metrics.
//

// Request can also use the REST API:
// monitoring.googleapis.com/v3/projects/NAME/timeSeries
//   ?aggregation.alignmentPeriod=60s
//   &aggregation.crossSeriesReducer=REDUCE_NONE
//   &aggregation.perSeriesAligner=ALIGN_RATE
//   &alt=json
//   &filter=metric.type+%3D+%22istio.io%2Fservice%2Fclient%2Frequest_count%22+AND+resource.type+%3D+%22istio_canonical_service%22+AND+resource.labels.namespace_name+%3D+%22fortio%22
//  &interval.endTime=2021-09-30T14%3A32%3A51-07%3A00
//  &interval.startTime=2021-09-30T14%3A27%3A51-07%3A00
//  &prettyPrint=false


type Stackdriver struct {
	projectID         string
	monitoringService *monitoring.Service
}

var (
	queryInterval = -5 * time.Minute
)

func NewStackdriver(projectID string) (*Stackdriver, error){
	monitoringService, err := monitoring.NewService(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get monitoring service: %v", err)
	}

	return &Stackdriver{projectID: projectID, monitoringService: monitoringService}, nil
}

func (s *Stackdriver) ListTimeSeries(ctx context.Context, namespace, resourceType, metricName, extra string) ([]*monitoring.TimeSeries, error) {
	endTime := time.Now()
	startTime := endTime.Add(queryInterval)

	f := fmt.Sprintf("metric.type = %q ",
		metricName)
	if resourceType != "" {
		f = f + fmt.Sprintf(" AND resource.type = %q", resourceType)
	}
	if namespace != "" {
		f = f + fmt.Sprintf(" AND resource.labels.namespace_name = %q", namespace)
	}
	if extra != "" {
		f = f + extra
	}

	lr := s.monitoringService.Projects.TimeSeries.List(fmt.Sprintf("projects/%v", s.projectID)).
		IntervalStartTime(startTime.Format(time.RFC3339)).
		IntervalEndTime(endTime.Format(time.RFC3339)).
		AggregationCrossSeriesReducer("REDUCE_NONE").
		AggregationAlignmentPeriod("60s").
		AggregationPerSeriesAligner("ALIGN_RATE").
		Filter(f).//, destCanonical
		Context(ctx)
	resp, err := lr.Do()
	if err != nil {
		return nil, err
	}
	if resp.HTTPStatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get expected status code from monitoring service, got: %d", resp.HTTPStatusCode)
	}

	return resp.TimeSeries, nil
}


