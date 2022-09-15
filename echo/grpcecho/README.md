# Minimal implementation for Istio Echo app, gRPC only

The intent is to evaluate the overhead of various gRPC options.

- 0.8M for a min go program
- 4.7M for an echo using HTTP.
- 9M - this server, only plain gRPC
- 20M - same app, but proxyless gRPC
- 22M - plus opencensus, prom, zpages, reflection

-	ocgrpc adds ~300k
