package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/costinm/hbone"
)

func InitMDS(hb *hbone.HBone) {
	hb.Mux.HandleFunc("/computeMetadata/v1/instance/service-accounts/", MDSHandler(hb))
}

// Adapter emulating MDS using an authenticator (K8S or GCP)
// Allows Envoy, gRPC to work without extra code to handle token exchanges.
// "aud" is the special provider returning access and audience tokens.
func MDSHandler(hb *hbone.HBone) func(writer http.ResponseWriter, request *http.Request) {
	return func(writer http.ResponseWriter, request *http.Request) {
		// Envoy request: Metadata-Flavor:[Google] X-Envoy-Expected-Rq-Timeout-Ms:[1000] X-Envoy-Internal:[true]
		pathc := strings.Split(request.URL.RawQuery, "=")
		if len(pathc) != 2 {
			log.Println("Token error", request.URL.RawQuery)
			writer.WriteHeader(500)
			return
		}
		aud := pathc[1]
		tp := hb.AuthProviders["gsa"]
		if tp == nil {
			tp = hb.AuthProviders["gcp"]
		}
		if tp == nil {
			writer.WriteHeader(500)
			return
		}
		tok, err := tp(context.Background(), aud)
		if err != nil {
			log.Println("Token error", err, pathc)
			writer.WriteHeader(500)
			return
		}
		writer.WriteHeader(200)
		log.Println(aud, tok)
		fmt.Fprintf(writer, "%s", tok)
	}
}
