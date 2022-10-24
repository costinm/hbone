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
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/instrument/syncfloat64"
	"go.opentelemetry.io/otel/metric/instrument/syncint64"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.12.0"
	"go.opentelemetry.io/otel/trace"
)

var _ http.Handler = &Handler{}

// Handler is http middleware that corresponds to the http.Handler interface and
// is designed to wrap a http.Transport (or equivalent), while individual routes on
// the mux are wrapped with WithRouteTag. A Handler will add various attributes
// to the span using the attribute.Keys defined in this package.
type Handler struct {
	operation string
	handler   http.Handler

	tracer trace.Tracer
	meter  metric.Meter

	propagators      propagation.TextMapPropagator
	spanStartOptions []trace.SpanStartOption

	readEvent  bool
	writeEvent bool

	filters []Filter

	spanNameFormatter func(string, *http.Request) string

	counters       map[string]syncint64.Counter
	valueRecorders map[string]syncfloat64.Histogram

	publicEndpoint   bool
	publicEndpointFn func(*http.Request) bool
}

func defaultHandlerFormatter(operation string, _ *http.Request) string {
	return operation
}

// NewHandler wraps the passed handler, functioning like middleware, in a span
// named after the operation and with any provided Options.
func NewHandler(handler http.Handler, operation string) http.Handler {
	h := Handler{
		handler:   handler,
		operation: operation,
	}

	h.createMeasures()

	return &h
}

func (h *Handler) configure(c *config) {
	h.tracer = c.Tracer
	h.meter = c.Meter
	h.propagators = c.Propagators
	h.spanStartOptions = c.SpanStartOptions
	h.readEvent = c.ReadEvent
	h.writeEvent = c.WriteEvent
	h.filters = c.Filters
	h.spanNameFormatter = c.SpanNameFormatter
	h.publicEndpoint = c.PublicEndpoint
	h.publicEndpointFn = c.PublicEndpointFn
}

func handleErr(err error) {
	if err != nil {
		otel.Handle(err)
	}
}

func (h *Handler) createMeasures() {
	// One of the worse naming conventions ever invented...
	// meter - creates instrumentProviders
	// each provider can create Counter, etc - with names.
	//
	h.counters = make(map[string]syncint64.Counter)
	h.valueRecorders = make(map[string]syncfloat64.Histogram)

	requestBytesCounter, err := h.meter.SyncInt64().Counter(RequestContentLength)
	handleErr(err)

	responseBytesCounter, err := h.meter.SyncInt64().Counter(ResponseContentLength)
	handleErr(err)

	serverLatencyMeasure, err := h.meter.SyncFloat64().Histogram(ServerLatency)
	handleErr(err)

	crequestBytesCounter, err := h.meter.SyncInt64().Counter(CRequestContentLength)
	handleErr(err)

	cresponseBytesCounter, err := h.meter.SyncInt64().Counter(CResponseContentLength)
	handleErr(err)

	cserverLatencyMeasure, err := h.meter.SyncFloat64().Histogram(CServerLatency)
	handleErr(err)

	h.counters[CRequestContentLength] = crequestBytesCounter
	h.counters[CResponseContentLength] = cresponseBytesCounter
	h.valueRecorders[CServerLatency] = cserverLatencyMeasure

	h.counters[RequestContentLength] = requestBytesCounter
	h.counters[ResponseContentLength] = responseBytesCounter
	h.valueRecorders[ServerLatency] = serverLatencyMeasure
}

// ServeHTTP serves HTTP requests (http.Handler).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestStartTime := time.Now()

	ctx := h.propagators.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	opts := h.spanStartOptions
	if h.publicEndpoint || (h.publicEndpointFn != nil && h.publicEndpointFn(r.WithContext(ctx))) {
		opts = append(opts, trace.WithNewRoot())
		// Linking incoming span context if any for public endpoint.
		if s := trace.SpanContextFromContext(ctx); s.IsValid() && s.IsRemote() {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: s}))
		}
	}

	opts = append([]trace.SpanStartOption{
		trace.WithAttributes(semconv.NetAttributesFromHTTPRequest("tcp", r)...),
		trace.WithAttributes(semconv.EndUserAttributesFromHTTPRequest(r)...),
		trace.WithAttributes(semconv.HTTPServerAttributesFromHTTPRequest(h.operation, "", r)...),
	}, opts...) // start with the configured options

	tracer := h.tracer

	if tracer == nil {
		if span := trace.SpanFromContext(r.Context()); span.SpanContext().IsValid() {
			tracer = newTracer(span.TracerProvider())
		} else {
			tracer = newTracer(otel.GetTracerProvider())
		}
	}

	ctx, span := tracer.Start(ctx, h.spanNameFormatter(h.operation, r), opts...)
	defer span.End()

	// "write" too
	//	span.AddEvent("read", trace.WithAttributes(ReadBytesKey.Int64(n)))

	labeler := &Labeler{}
	ctx = injectLabeler(ctx, labeler)

	h.handler.ServeHTTP(w, r.WithContext(ctx))

	tattributes := []attribute.KeyValue{}

	// TODO: Consider adding an event after each read and write, possibly as an
	// option (defaulting to off), so as to not create needlessly verbose spans.
	//if read > 0 {
	//	attributes = append(attributes, ReadBytesKey.Int64(read))
	//}
	//if rerr != nil && rerr != io.EOF {
	//	attributes = append(attributes, ReadErrorKey.String(rerr.Error()))
	//}
	//if wrote > 0 {
	//	attributes = append(attributes, WroteBytesKey.Int64(wrote))
	//}
	//if statusCode > 0 {
	//	attributes = append(attributes, semconv.HTTPAttributesFromHTTPStatusCode(statusCode)...)
	//	span.SetStatus(semconv.SpanStatusFromHTTPStatusCodeAndSpanKind(statusCode, trace.SpanKindServer))
	//}
	//if werr != nil && werr != io.EOF {
	//	attributes = append(attributes, WriteErrorKey.String(werr.Error()))
	//}
	//span.SetAttributes(semconv.HTTPClientAttributesFromHTTPRequest(r)...)

	span.SetAttributes(tattributes...)

	// Add metrics
	attributes := append(labeler.Get(), semconv.HTTPServerMetricAttributesFromHTTPRequest(h.operation, r)...)
	//h.counters[RequestContentLength].Add(ctx, bw.read, attributes...)
	//h.counters[ResponseContentLength].Add(ctx, rww.written, attributes...)

	// Use floating point division here for higher precision (instead of Millisecond method).
	elapsedTime := float64(time.Since(requestStartTime)) / float64(time.Millisecond)

	h.valueRecorders[ServerLatency].Record(ctx, elapsedTime, attributes...)
}

// WithRouteTag annotates a span with the provided route name using the
// RouteKey Tag.
func WithRouteTag(route string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		span := trace.SpanFromContext(r.Context())
		span.SetAttributes(semconv.HTTPRouteKey.String(route))
		h.ServeHTTP(w, r)
	})
}
