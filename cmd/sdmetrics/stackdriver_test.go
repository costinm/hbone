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
	"log"
	"os"
	"testing"
)

// TestStackdriver should be run at the end, after tests generating load.
// Will check the metrics have the labels and non zero value.
// See ../istio/tests/integration/telemetry/stackdriver/stackdriver_filter_test.go
func TestStackdriver(t *testing.T) {
	projectID := os.Getenv("PROJECT_ID")
	if projectID == "" {
		
		t.Skip("Missing PROJECT_ID")
		return
	}

	sd, err := NewStackdriver(projectID)
	if err != nil {
		t.Fatal(err)
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
	ts, err := sd.ListTimeSeries(context.Background(), "fortio", "istio_canonical_service",
		"istio.io/service/client/request_count",
		" AND metric.labels.source_canonical_service_name = \"fortio\"" +
		" AND metric.labels.response_code = \"200\"")
	if err != nil {
		t.Fatal(err)
	}

	for _, tsr := range ts {
		log.Println(tsr.Metric.Labels)
		v := tsr.Points[0].Value
		log.Println(*v.DoubleValue)

	}

	// TODO: verify CR server metrics have expected client and server labels


	// TODO: same for CR->Pod traffic

	//ts, err = sd.ListTimeSeries(context.Background(), "fortio", "istio_canonical_service",
	//	"istio.io/service/client/request_count", "fortio-cr")
	//if err != nil {
	//	t.Fatal(err)
	//}
	//
	//for _, tsr := range ts {
	//	log.Println(tsr.Metric.Labels)
	//	v := tsr.Points[0].Value
	//	log.Println(*v.DoubleValue)
	//
	//}
}
