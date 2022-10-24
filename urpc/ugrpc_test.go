package uxds

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/nio"
	auth "github.com/costinm/meshauth"
)

func TestGRPC(t *testing.T) {
	_, cf := context.WithTimeout(context.Background(), 10*time.Second)
	defer cf()

	// New self-signed root CA
	ca := auth.NewCA("cluster.local")
	// Sign the certs and create the identities for the 2 workloads.
	aliceID := ca.NewID("alice", "default")
	bobID := ca.NewID("bob", "default")

	alice := hbone.New(aliceID, nil)
	bob := hbone.New(bobID, nil)

	bob.Mux.Handle("/google.security.meshca.v1.MeshCertificateService/CreateCertificate",
		nil)

	l, err := nio.ListenAndServeTCP(laddr(":14108"), bob.HandleAcceptedH2)
	if err != nil {
		t.Fatal(err)
	}
	bobHBAddr := l.Addr().String()

	// Configure Alice with bob's information.
	alice.AddService(&hbone.Cluster{Addr: "default.bob:8080"},
		&hbone.Endpoint{HBoneAddress: bobHBAddr})

}

func laddr(addr string) string {
	if os.Getenv("NO_FIXED_PORTS") != "" {
		return ":0"
	}
	return addr
}
