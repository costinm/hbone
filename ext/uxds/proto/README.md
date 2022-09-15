# XDS and Istio subset

Envoy and Istio protos are very large and have many dependencies. This is a subset - a bit 
larger than what is used, but with no external deps.

In particular 'Any', 'Struct' are redefined and used as raw protocols - no deep unmarshalling.

## Usage

grpcurl -protoset <(buf build -o -) ...

