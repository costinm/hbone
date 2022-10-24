# Proto buffers used in this repo

Proto buffers are commonly used - XDS, certs, OTel, OpenMetrics, etc.
Unfortunately the client libraries tend to be huge and bring many
dependencies. 

To avoid large dependencies and keep size small, a different approach 
is used for protos in this repo: it includes only a subset, extracted
from the original and sometimes trimmed down. 

For example, for XDS only the core proto and the object used are included - 
envoy has a huge collection of protos, matching its internal implementation.
Similarly for the other integrations cleaned up and minimized protos are added.

# XDS and Istio subset

Envoy and Istio protos are very large and have many dependencies. This is a subset - a bit
larger than what is used, but with no external deps.

In particular 'Any', 'Struct' are redefined and used as raw protocols - no deep unmarshalling.

## Buf

grpcurl -protoset <(buf build -o -) ...

