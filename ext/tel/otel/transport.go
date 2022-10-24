// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package otel

import (
	"context"
	"net/http"
	"net/http/httptrace"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/instrument/syncfloat64"
	"go.opentelemetry.io/otel/metric/instrument/syncint64"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.12.0"
	"go.opentelemetry.io/otel/trace"
)

// Transport implements the http.RoundTripper interface and wraps
// outbound HTTP(S) requests with a span.
type Transport struct {
	rt http.RoundTripper

	tracer            trace.Tracer
	propagators       propagation.TextMapPropagator
	spanStartOptions  []trace.SpanStartOption
	filters           []Filter
	spanNameFormatter func(string, *http.Request) string
	clientTrace       func(context.Context) *httptrace.ClientTrace
	counters          map[string]syncint64.Counter
	valueRecorders    map[string]syncfloat64.Histogram
	meter             metric.Meter
}

var _ http.RoundTripper = &Transport{}

// NewTransport wraps the provided http.RoundTripper with one that
// starts a span and injects the span context into the outbound request headers.
//
// If the provided http.RoundTripper is nil, http.DefaultTransport will be used
// as the base http.RoundTripper.
func NewTransport(base http.RoundTripper) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}

	t := Transport{
		rt: base,
	}

	t.createMeasures()

	return &t
}

func (h *Transport) createMeasures() {
	h.counters = make(map[string]syncint64.Counter)
	h.valueRecorders = make(map[string]syncfloat64.Histogram)
}

func (t *Transport) applyConfig(c *config) {
	t.tracer = c.Tracer
	t.propagators = c.Propagators
	t.spanStartOptions = c.SpanStartOptions
	t.filters = c.Filters
	t.spanNameFormatter = c.SpanNameFormatter
	t.clientTrace = c.ClientTrace
	t.meter = c.Meter
}

func defaultTransportFormatter(_ string, r *http.Request) string {
	return "HTTP " + r.Method
}

// RoundTrip creates a Span and propagates its context via the provided request's headers
// before handing the request to the configured base RoundTripper. The created span will
// end when the response body is closed or when a read from the body returns io.EOF.
func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	requestStartTime := time.Now()
	tracer := t.tracer

	if tracer == nil {
		if span := trace.SpanFromContext(r.Context()); span.SpanContext().IsValid() {
			tracer = newTracer(span.TracerProvider())
		} else {
			tracer = newTracer(otel.GetTracerProvider())
		}
	}

	opts := append([]trace.SpanStartOption{}, t.spanStartOptions...) // start with the configured options

	ctx, span := tracer.Start(r.Context(), t.spanNameFormatter("", r), opts...)

	if t.clientTrace != nil {
		ctx = httptrace.WithClientTrace(ctx, t.clientTrace(ctx))
	}

	r = r.WithContext(ctx)
	span.SetAttributes(semconv.HTTPClientAttributesFromHTTPRequest(r)...)
	t.propagators.Inject(ctx, propagation.HeaderCarrier(r.Header))

	labeler := &Labeler{}
	ctx = injectLabeler(ctx, labeler)

	res, err := t.rt.RoundTrip(r)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return res, err
	}

	span.SetAttributes(semconv.HTTPAttributesFromHTTPStatusCode(res.StatusCode)...)
	span.SetStatus(semconv.SpanStatusFromHTTPStatusCode(res.StatusCode))

	//res.Body = newWrappedBody(span, res.Body)

	// Add metrics
	attributes := append(labeler.Get(), semconv.HTTPServerMetricAttributesFromHTTPRequest("client", r)...)

	//if s, ok := res.Body.(*h2.Stream); ok {
	//	s.OnDone(func(stream *h2.Stream) {
	//t.counters[CResponseContentLength].Add(ctx, int64(s.RcvdBytes), attributes...)
	//t.counters[CRequestContentLength].Add(ctx, int64(s.SentBytes), attributes...)

	// TODO
	//span.RecordError(err)
	//span.SetStatus(codes.Error, err.Error())

	span.End()
	//	})
	//}
	//t.counters[CRequestContentLength].Add(ctx, bw.read, attributes...)

	// Use floating point division here for higher precision (instead of Millisecond method).
	elapsedTime := float64(time.Since(requestStartTime)) / float64(time.Millisecond)

	t.valueRecorders[CServerLatency].Record(ctx, elapsedTime, attributes...)

	return res, err
}
