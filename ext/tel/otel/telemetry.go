package otel

import (
	"net/http"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/h2"
	"github.com/costinm/hbone/nio"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

type PromWatcher struct {
}

var (
	// Create a new registry.
	reg = prometheus.NewRegistry()

	ns     = "hbone"
	Active = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns,
		Subsystem: "h2",
		Name:      "active",
		Help:      "",
	})
	Requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns,
		Subsystem: "h2",
		Name:      "http_request",
		Help:      "",
	}, []string{})
)

// MetricVal extracts the value and labels of a metric.
// Unfortunately the default impl hides it.
// Useful for tests and alternative uses.
// TODO: fork ?
func MetricVal(m prometheus.Metric) (float64, []*dto.LabelPair) {
	dtom := &dto.Metric{}
	m.Write(dtom)
	return MetricProtoVal(dtom)
}

func MetricProtoVal(dtom *dto.Metric) (float64, []*dto.LabelPair) {
	if dtom.Counter != nil {
		return *dtom.Counter.Value, dtom.Label
	}
	if dtom.Gauge != nil {
		return *dtom.Gauge.Value, dtom.Label
	}
	return 0, dtom.Label
}

func MetricProtoLabels(dtol []*dto.LabelPair) map[string]string {
	res := map[string]string{}
	for _, dtole := range dtol {
		res[*dtole.Name] = *dtole.Value
	}
	return res
}

func MetricList(reg *prometheus.Registry, mn string) []*dto.Metric {
	ml := []*dto.Metric{}
	mf, _ := reg.Gather()
	for _, mfe := range mf {
		if *mfe.Name != mn {
			continue
		}
		for _, mm := range mfe.Metric {
			ml = append(ml, mm)
		}
	}
	return ml
}

func InitProm(hb *hbone.HBone) {

	// Add Go module build info.
	reg.MustRegister(collectors.NewBuildInfoCollector())
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	reg.MustRegister(Active, Requests)

	Latencies := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns,
		Subsystem: "h2",
		Name:      "http_request_lat",
		Help:      "",
	}, []string{})

	reg.MustRegister(Latencies)

	hb.OnEvent(h2.EventStreamStart, h2.EventHandlerFunc(func(evt h2.EventType, t *h2.H2Transport, s *h2.H2Stream, f *nio.Buffer) {
		Active.Inc()
	}))

	hb.OnEvent(h2.EventStreamClosed, h2.EventHandlerFunc(func(evt h2.EventType, t *h2.H2Transport, s *h2.H2Stream, f *nio.Buffer) {
		Active.Dec()
		Requests.WithLabelValues().Inc()
	}))

	// Expose the registered metrics via HTTP.
	http.Handle("/metrics", promhttp.HandlerFor(
		reg,
		promhttp.HandlerOpts{
			// Opt into OpenMetrics to support exemplars.
			EnableOpenMetrics: true,
		},
	))

}
