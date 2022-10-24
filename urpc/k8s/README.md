# K8S helpers

HBone package integrates with K8S - it supports K8S Token auth, mainly to authenticate to Istiod
or other services requiring JWT auth. It can also auto-configure by using a config map.

To keep minimal dependencies this uses the K8S REST API directly. 
