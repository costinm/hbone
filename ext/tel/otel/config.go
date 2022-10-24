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

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const (
	instrumentationName = "github.com/costinm/hbone/ext/otel"
)

// config represents the configuration options available for the http.Handler
// and http.Transport types.
type config struct {
	Tracer            trace.Tracer
	Meter             metric.Meter
	Propagators       propagation.TextMapPropagator
	SpanStartOptions  []trace.SpanStartOption
	PublicEndpoint    bool
	PublicEndpointFn  func(*http.Request) bool
	ReadEvent         bool
	WriteEvent        bool
	Filters           []Filter
	SpanNameFormatter func(string, *http.Request) string
	ClientTrace       func(context.Context) *httptrace.ClientTrace

	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
}
