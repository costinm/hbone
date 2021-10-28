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
	"fmt"
	"net/http"
	"time"

	monitoring "google.golang.org/api/monitoring/v3"
)

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

func main() {

}

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

	f := fmt.Sprintf("metric.type = %q AND resource.type = %q AND resource.labels.namespace_name = %q",
		metricName,
		resourceType,
		namespace)
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

