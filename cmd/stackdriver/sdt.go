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
	"encoding/json"
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

	r = flag.String("r", "", "Resource type.")

	ns     = flag.String("n", os.Getenv("WORKLOAD_NAMESPACE"), "Namespace")
	wname  = flag.String("s", os.Getenv("WORKLOAD_NAME"), "Service name")
	rev    = flag.String("v", "", "Version/revision")
	metric = flag.String("m", "istio.io/service/client/request_count", "Metric name")
	extra  = flag.String("x", "", "Extra query parameters")

	includeZero = flag.Bool("zero", false, "Include metrics with zero value")
	jsonF       = flag.Bool("json", false, "json output")
)

func main() {
	flag.Parse()
	projectID := *p
	if *p == "" {
		projectID = "wlhe-cr"
		//panic("Missing PROJECT_ID")
		//return
	}

	ctx, cf := context.WithTimeout(context.Background(), 10*time.Second)
	defer cf()

	sd, err := NewStackdriver(projectID)
	if err != nil {
		panic(err)
	}

	if *r == "-" {
		rl, err := sd.ListResources(ctx, *ns, *metric, *extra)
		if err != nil {
			log.Fatalf("Error %v", err)
		}
		for _, tsr := range rl {
			log.Println(tsr)
		}
		return
	}

	// Verify client side metrics (in pod) reflect the CloudrRun server properties
	ts, err := sd.ListTimeSeries(ctx,
		*ns, *r,
		*metric, *extra)
	//" AND metric.labels.source_canonical_service_name = \"fortio\"" +
	//		" AND metric.labels.response_code = \"200\"")
	if err != nil {
		log.Fatalf("Error %v", err)
	}

	for _, tsr := range ts {
		v := tsr.Points[0].Value
		if !*includeZero && *v.DoubleValue == 0 {
			continue
		}
		if *jsonF {
			d, _ := json.Marshal(tsr)
			fmt.Println(string(d))
		} else {
			fmt.Printf("%v %v\n", *v.DoubleValue, tsr.Metric.Labels)
		}
	}
}

type Stackdriver struct {
	projectID         string
	monitoringService *monitoring.Service
}

var (
	queryInterval = -5 * time.Minute
)

func NewStackdriver(projectID string) (*Stackdriver, error) {
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

	// Alternative: use REDUCE_MEAN
	// Aligh on 5m

	// ALIGN_NONE to see the raw data points - how many samples where sent
	lr := s.monitoringService.Projects.TimeSeries.List(fmt.Sprintf("projects/%v", s.projectID)).
		IntervalStartTime(startTime.Format(time.RFC3339)).
		IntervalEndTime(endTime.Format(time.RFC3339)).
		AggregationCrossSeriesReducer("REDUCE_NONE").
		AggregationAlignmentPeriod("60s").
		AggregationPerSeriesAligner("ALIGN_RATE").
		Filter(f). //, destCanonical
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

// For a metric, list resource types that generated the metric and the names.
func (s *Stackdriver) ListResources(ctx context.Context, namespace, metricName, extra string) ([]*monitoring.TimeSeries, error) {
	endTime := time.Now()
	startTime := endTime.Add(queryInterval)

	f := fmt.Sprintf("metric.type = %q ", metricName)
	if namespace != "" {
		f = f + fmt.Sprintf(" AND resource.labels.namespace_name = %q", namespace)
	}
	if extra != "" {
		f = f + extra
	}

	lr := s.monitoringService.Projects.TimeSeries.
		List(fmt.Sprintf("projects/%v", s.projectID)).
		IntervalStartTime(startTime.Format(time.RFC3339)).
		IntervalEndTime(endTime.Format(time.RFC3339)).
		AggregationCrossSeriesReducer("REDUCE_NONE").
		AggregationAlignmentPeriod("60s").
		AggregationPerSeriesAligner("ALIGN_RATE").
		Filter(f). //, destCanonical
		Context(ctx)
	resp, err := lr.Do()
	if err != nil {
		return nil, err
	}
	if resp.HTTPStatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get expected status code from monitoring service, got: %d", resp.HTTPStatusCode)
	}

	rtype := map[string]map[string]string{}

	for _, t := range resp.TimeSeries {
		log.Println(t)
		rtm := rtype[t.Resource.Type]
		if rtm == nil {
			rtype[t.Resource.Type] = t.Resource.Labels
		} else {
			// Check if any label is different, log
			for k, _ := range t.Resource.Labels {
				if rtm[k] == "" {
					log.Println(t.Resource.Type, k, rtm, t.Resource.Labels)
				}
			}
			for k, _ := range rtm {
				if t.Resource.Labels[k] == "" {
					log.Println(t.Resource.Type, k, rtm, t.Resource.Labels)
				}
			}
		}
	}

	return resp.TimeSeries, nil
}
