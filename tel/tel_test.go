package tel

import (
	"expvar"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestTelemetry(t *testing.T) {

	// Using raw expvar interface - note that it will fail if registered in 2 libraries.
	counter := expvar.NewFloat("example_counter_total")
	counter.Add(1)

	// Using the metric in code
	val := MetricValue("example_counter_total")
	if val != 1 {
		t.Errorf("Expectig %d got %d", 1, val)
	}

	// Metric with labels - using a map. Will fail if registered in 2 libraries
	withLabels := expvar.NewMap("with_labels")
	c1 := &expvar.Float{}
	withLabels.Set("a=\"1\"", c1)
	c1.Add(1)

	c2 := &expvar.Float{}
	withLabels.Set("b=\"1\"", c2)
	c2.Add(2)

	// Simpler, works for existing metrics (Float type)
	counter1 := Get("example_counter_total")
	counter1.Add(1)

	counterLabels := Get("with_lables_total", "a", "1")
	counterLabels.Add(1)

	counterHist := &Histogram{}
	counterHist.Update(32)

	// Generate the metrics
	w := httptest.NewRecorder()
	HandleMetrics(w, &http.Request{})

	// Parse the metrics into a map
	mlines := strings.Split(w.Body.String(), "\n")
	metrics := map[string]float64{}
	for _, k := range mlines {
		kv := strings.Split(k, " ")
		if len(kv) > 1 {
			vf, err := strconv.ParseFloat(kv[1], 64)
			if err == nil {
				metrics[kv[0]] = vf
			}
		}
	}

	if metrics["example_counter_total"] != 2 {
		t.Error(metrics)
	}
	//log.Println(w.Body.String())
}
