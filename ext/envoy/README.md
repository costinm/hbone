# Envoy tests

Assume an agent is running, /etc/certs is updated. Using HBone protocol in envoy. 
All tests expect an iperf3 server running on localhost. The test env will open a number of ports
that all forward to iperf3 with different configs and paths.

## Ports:

Envoy client: non inbound hbone, to keep config simple
- admin: 13000
-


Envoy server: no outbound hbone, to keep config simple.
- admin: 14000
- hbone: 14008
- inner_mtls: 14006 - accepts mTLS, forwards to iperf3

Envoy Gateway/PEP: (TODO)
- admin: 12000
- hbone: 12008


## 
