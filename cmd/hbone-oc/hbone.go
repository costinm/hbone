//Copyright 2021 Google LLC
//
//Licensed under the Apache License, Version 2.0 (the "License");
//you may not use this file except in compliance with the License.
//You may obtain a copy of the License at
//
//    https://www.apache.org/licenses/LICENSE-2.0
//
//Unless required by applicable law or agreed to in writing, software
//distributed under the License is distributed on an "AS IS" BASIS,
//WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//See the License for the specific language governing permissions and
//limitations under the License.

package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	ocprom "contrib.go.opencensus.io/exporter/prometheus"
	"github.com/costinm/hbone"
	"github.com/costinm/hbone/auth"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"go.opencensus.io/plugin/ochttp"
	"go.opencensus.io/plugin/runmetrics"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/zpages"
)

func init() {
	if err := view.Register(ochttp.DefaultServerViews...); err != nil {
		log.Println("Failed to register ocgrpc server views: %v", err)
	}
	if err := view.Register(ochttp.DefaultClientViews...); err != nil {
		log.Println("Failed to register ocgrpc server views: %v", err)
	}

	// Similar with pilot-agent
	registry := prometheus.NewRegistry()
	wrapped := prometheus.WrapRegistererWithPrefix("sshca_",
		prometheus.Registerer(registry))
	wrapped.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	wrapped.MustRegister(collectors.NewGoCollector())

	exporter, err := ocprom.NewExporter(ocprom.Options{Registry: registry,
		Registerer: wrapped})
	if err != nil {
		log.Fatalf("could not setup exporter: %v", err)
	}
	view.RegisterExporter(exporter)
}

// Same as hbone-min, plus OpenCensus instrumentation
// Used for testing size impact of OC as well as perf testing, as client (-L )
func main() {
	a := auth.NewMeshAuth()
	hb := hbone.New(a)
	hb.Transport = func(t http.RoundTripper) http.RoundTripper {
		return &ochttp.Transport{}
	}
	hb.HandlerWrapper = func(h http.Handler) http.Handler {
		return &ochttp.Handler{Handler: h}
	}
	mux := &http.ServeMux{}
	zpages.Handle(mux, "/debug")

	runmetrics.Enable(runmetrics.RunMetricOptions{
		EnableCPU:    true,
		EnableMemory: true,
		Prefix:       "hbone/",
	})

	dest := "127.0.0.1:15008"
	lp := ":12001"
	if len(os.Args) > 1 {
		dest = os.Args[0]
	}
	err := hbone.LocalForwardPort(lp, dest, hb)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error forwarding ", err)
		log.Fatal(err)
	}
}
