package handlers

import (
	"expvar"
	"fmt"
	"net/http"
	"sync"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/h2"
	"github.com/costinm/hbone/nio"
)

func MetricValue(p expvar.Var) int64 {
	switch p.(type) {
	case *expvar.Int:
		return p.(*expvar.Int).Value()
	}
	return 0
}

type ExpVar struct {
	vars      sync.Map // map[string]Var
	varKeysMu sync.RWMutex
}

func (ev *ExpVar) expvarHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fmt.Fprintf(w, "{\n")
	first := true

	ev.varKeysMu.RLock()
	defer ev.varKeysMu.RUnlock()

	ev.vars.Range(func(key, value any) bool {
		if !first {
			fmt.Fprintf(w, ",\n")
		}
		v, _ := value.(expvar.Var)

		first = false
		fmt.Fprintf(w, "%q: %s", key, v.String())
		return true
	})

	fmt.Fprintf(w, "\n}\n")
}

func InitExpvar(hb *hbone.HBone) {

	hb.OnEvent(h2.EventStreamStart, h2.EventHandlerFunc(func(evt h2.EventType, t *h2.H2Transport, s *h2.H2Stream, f *nio.Buffer) {
	}))

	hb.OnEvent(h2.EventStreamClosed, h2.EventHandlerFunc(func(evt h2.EventType, t *h2.H2Transport, s *h2.H2Stream, f *nio.Buffer) {
	}))

	// WIP: write expvar metrics using prometheus format (text)
	http.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
		expvar.Do(func(kv expvar.KeyValue) {

		})
	})
}
