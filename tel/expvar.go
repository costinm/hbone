package tel

import (
	"expvar"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Golang native expvar support.
// By importing the package, a handler for /debug/vars is created, returning JSON
//
// Prometheus conventions used for expvar names:
// [a-zA-Z0-9:_]*{key="value"}

// The expvar package provides built-in support for metrics. They are exposed as
// json - this package provides a prometheus exporter and helpers for use at runtime,
// for tests or for adjusting behavior based on telemetry.
//
// Primary interface is Var, with String() returning json. Pre-defined Var:
// - Int
// - Float
//
// - Map - includes a sorted keys, can be used as a var - and with Do().
// - String
// - Func - anything, will be marshalled as json
//
// Publish is used to create vars in the package-local 'vars' - it provides
// Get(), Do() - but not Delete.
//
// Each pre-defined var is used with NewXXX(name) - which creates and Publish()

// Naming:
//  namespace_NAME_type
// - seconds
// - bytes
// - _total - all counters - rest are gauges or complex types.
// - sum, count, bucket - in histo and summaries
// - seconds_total
// - info
// - ratio
// - timestamp_seconds
//
// namespace is app name or 'http', etc
// unit is sec, bytes, etc
//
// Labels can be the ID, service, etc.
// - instance
// - job
// - endpoint
// - method
// - status_code
//
// In prom values are float

// TODO: Histogram: _count, _sum metrics with
// _bucket metric, type counter, and le=xx label

// TODO: periodic clean of unused labels. Need special kind of map.

func Get(name string, labels ...string) *expvar.Float {
	c1 := expvar.Get("example_counter_total")
	if c1 == nil {
		c1 = &expvar.Float{}
		expvar.Publish("example_counter_total", c1)
	}
	return c1.(*expvar.Float)
}

// Given an expvar, extract the int value.
func MetricValue(name string) int64 {
	p := expvar.Get(name)
	if p == nil {
		return 0
	}
	switch p.(type) {
	case *expvar.Int:
		return p.(*expvar.Int).Value()
	case *expvar.Float:
		return int64(p.(*expvar.Float).Value())
	}
	return 0
}

func MetricValues(name string) map[string]int64 {
	p := expvar.Get(name)
	if p == nil {
		return nil
	}

	if m, ok := p.(*expvar.Map); ok {
		res := map[string]int64{}
		m.Do(func(kv1 expvar.KeyValue) {
			v := kv1.Value
			switch v.(type) {
			case *expvar.Int:
				res[kv1.Key] = v.(*expvar.Int).Value()
			case *IntExp:
				res[kv1.Key] = v.(*IntExp).Value()
			}
		})
		return res
	}
	return nil
}

type IntExp struct {
	expvar.Int
	LastUse time.Time
}

func (v *IntExp) Add(delta int64) {
	v.Int.Add(delta)
	v.LastUse = time.Now()
}

func (v *IntExp) Set(delta int64) {
	v.Int.Set(delta)
	v.LastUse = time.Now()
}

func WithLabels(name, l string) *expvar.Int {
	m := expvar.Get(name)
	var mp *expvar.Map
	if m == nil {
		mp = expvar.NewMap(name)
	}
	mp.Add(l, 0)
	v := mp.Get(l)
	return v.(*expvar.Int)
}

type ExpVar struct {
	vars      sync.Map // map[string]Var
	varKeysMu sync.RWMutex
}

func HandleMetrics(w http.ResponseWriter, req *http.Request) {
	expvar.Do(func(kv expvar.KeyValue) {
		// # HELP node_memory_used_bytes Total memory used in the node in bytes
		//# TYPE node_memory_used_bytes gauge
		if m, ok := kv.Value.(*expvar.Map); ok {
			m.Do(func(kv1 expvar.KeyValue) {
				v := kv1.Value
				switch a := v.(type) {
				case (*expvar.Int):
					fmt.Fprintf(w, "%s{%s} %d\n", kv.Key, kv1.Key, a.Value())
				case (*expvar.Float):
					fmt.Fprintf(w, "%s{%s} %f\n", kv.Key, kv1.Key, a.Value())
				default:
					//fmt.Fprintf(w, "%s{%s} %q\n", kv.Key, kv1.Key, kv1.Value.String())
				}
			})
		} else {
			v := kv.Value
			switch v.(type) {
			case (*expvar.Int):
				fmt.Fprintf(w, "%s %d\n", kv.Key, v.(*expvar.Int).Value())
			case (*expvar.Float):
				fmt.Fprintf(w, "%s %f\n", kv.Key, v.(*expvar.Float).Value())
			default:
				//fmt.Fprintf(w, "%s %q\n", kv.Key, kv.Value.String())
			}
		}
	})
	writeProcessMetrics(w)
	writeIOMetrics(w)
	writeFDMetrics(w)
	writeProcessMetrics(w)
}
