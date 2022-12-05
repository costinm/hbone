# Telemetry support

This is a minimal prometheus exporter, using expvar and a histogram forked from VictoriaMetrics package.

No extenernal dependencies, only minimum code to get expvar into prometheus.

Rationale:

1. Prometheus official client has too many depsn and is over complex. 
2. Same for otel client
3. In both it is not easy to get the metric values from code for testing or metric-based decisions.
4. VictoriaMetrics removes most deps and complexity - but still invents its own Counter, etc.

The expvar package is not perfect - but 'good enough', there is not a lot of value in having 4 different
ways to register an atomic var.

## Expvar use 

Inspired from VictoriaMetrics, the naming of expvar is required to match prometheus naming conventions.


The expvar API does not allow removal of a registered metric - however it allows returning an existing
one, and allows removal from an expvar.Map.


## Usage

Use expvar normally, but name metrics using prometheus style. 

To export as prometheus text - use tel.HandleMetrics

To use a historgram:

```go

hist := tel.GetHistorgram("example", "a=1")


```
