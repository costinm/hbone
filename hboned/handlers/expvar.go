package handlers

import (
	"log"
	"net/http"
	"time"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/h2"
	"github.com/costinm/hbone/nio"
	"github.com/costinm/hbone/tel"
)

func InitExpvar(hb *hbone.HBone) {

	hb.OnEvent(h2.EventStreamStart, h2.EventHandlerFunc(func(evt h2.EventType, t *h2.H2Transport, s *h2.H2Stream, f *nio.Buffer) {
	}))

	hb.OnEvent(h2.EventStreamClosed, h2.EventHandlerFunc(func(evt h2.EventType, t *h2.H2Transport, s *h2.H2Stream, f *nio.Buffer) {
		// TODO: Access log format, json, proto ?
		log.Println("Stream", s.Request.URL, time.Since(s.Open))
	}))

	// WIP: write expvar metrics using prometheus format (text)
	http.HandleFunc("/metrics", tel.HandleMetrics)
}
