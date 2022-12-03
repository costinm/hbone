package grpcecho

import (
	"context"
	"log"
	"testing"
	"time"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/nio"
	"github.com/costinm/hbone/urpc"
	"github.com/costinm/hbone/urpc/gen/proto"
	auth "github.com/costinm/meshauth"
)

func TestGRPC(t *testing.T) {
	ctx, cf := context.WithTimeout(context.Background(), 10000*time.Second)
	defer cf()

	// New self-signed root CA
	ca := auth.NewCA("cluster.local")

	// Setup Bob the server.
	bobID := ca.NewID("bob", "default")
	bobID.AllowedNamespaces = []string{"*"}
	bob := hbone.New(bobID, nil)

	//bob.Mux.Handle("/google.security.meshca.v1.MeshCertificateService/CreateCertificate", nil)
	eh := &EchoGrpcHandler{
		Port:         0,
		Version:      "",
		Cluster:      "",
		IstioVersion: "1",
		Mesh:         bob,
	}
	bob.Mux.Handle(ECHO_SERVICE, eh)
	bob.Mux.Handle("/proto.EchoTestService/ForwardEcho", eh)

	l, err := nio.ListenAndServeTCP(":14108", bob.HandleAcceptedH2)
	if err != nil {
		t.Fatal(err)
	}
	bobHBAddr := l.Addr().String()

	// Setup Alice the client
	aliceID := ca.NewID("alice", "default")
	alice := hbone.New(aliceID, nil)

	// Configure Alice with bob's information.
	alice.AddService(&hbone.Cluster{Addr: "default.bob:8080"},
		&hbone.Endpoint{HBoneAddress: bobHBAddr})

	c, _ := alice.Cluster(ctx, "default.bob:8080")
	urpc, err := urpc.New(ctx, c, "default.bob:8080", ECHO_SERVICE)
	if err != nil {
		t.Fatal("Error connecting", err)
	}
	er := &proto.EchoResponse{}
	urpc.Invoke(&proto.EchoRequest{
		Message: "hi",
	}, er)

	log.Println(er)
}
