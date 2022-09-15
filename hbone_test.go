package hbone

import (
	"context"
	"io"
	"log"
	"net"
	"os"
	"testing"
	"time"

	"github.com/costinm/hbone/auth"
	"github.com/costinm/hbone/echo"
	"github.com/costinm/hbone/nio"
)

func listenAndServeTCP(addr string, f func(conn net.Conn)) (net.Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	go nio.ServeListener(listener, f)
	return listener, nil
}

func laddr(addr string) string {
	if os.Getenv("NO_FIXED_PORTS") != "" {
		return ":0"
	}
	return addr
}

// WIP
func TestHBone(t *testing.T) {
	ctx, cf := context.WithTimeout(context.Background(), 10*time.Second)
	defer cf()

	// New self-signed root CA
	ca := auth.NewCA("cluster.local")
	// Sign the certs and create the identities for the 2 workloads.
	aliceID := ca.NewID("alice", "default")
	bobID := ca.NewID("bob", "default")

	// Create the mesh node based on the identity (directly from CA).
	alice := New(aliceID)
	aliceID.AllowedNamespaces = []string{"*"}

	// Normal process for loading identity:
	bobID2 := auth.NewMeshAuth()
	bobID2.TrustedCertPool.AddCert(ca.CACert)
	bobID2.SetTLSCertificate(bobID.Cert)
	bobID2.AllowedNamespaces = []string{"*"}

	bob := New(bobID2)

	// Start Bob's servers for testing.
	// The H2 port is used to test serverless/trusted-net mode where TLS is terminated by another proxy.
	l, err := listenAndServeTCP(laddr(":14108"), bob.HandleAcceptedH2)
	if err != nil {
		t.Fatal(err)
	}
	bobHBAddr := l.Addr().String()

	//lc, err := listenAndServeTCP(":0", bob.HandleAcceptedH2C)
	//if err != nil {
	//	t.Fatal(err)
	//}
	//bobHBCAddr := lc.Addr().String()

	// Start an echo handler on bob. Equivalent with a pod listening on that port.
	// The service is 'default.bob:8080'
	eh := &echo.EchoHandler{Debug: Debug}
	ehL, err := eh.Start(laddr(":14130"))
	if err != nil {
		t.Fatal(err)
	}
	// The 'pod address' for the echo handler
	bobEndpoint := ehL.Addr().String()

	// Configure Alice with bob's information.
	alice.AddService(&Cluster{Addr: "default.bob:8080"},
		&Endpoint{Address: bobEndpoint, HBoneAddress: bobHBAddr})

	// Configure Alice with an endpooint using 'http proxy' mode.
	alice.AddService(&Cluster{Addr: "default-tun.bob:8080"}, &Endpoint{Address: bobEndpoint, HBoneAddress: bobHBAddr,
		Labels: map[string]string{"http_proxy": "POST://"}})

	// Alice opens hbone to TCP connection to bob's echo server.
	t.Run("alice-bob", func(t *testing.T) {
		nc, err := alice.DialContext(ctx, "", "default.bob:8080")
		if err != nil {
			t.Fatal(err)
		}

		EchoClient2(t, nc, nc)
	})

	// TUN mode - serverless.
	t.Run("alice-bob-tun", func(t *testing.T) {
		nc, err := alice.DialContext(ctx, "", "default-tun.bob:8080")
		if err != nil {
			t.Fatal(err)
		}

		EchoClient2(t, nc, nc)
	})

	t.Run("server-close", func(t *testing.T) {
		for _, a := range []string{"default.bob:8080", "default-tun.bob:8080"} {
			//nc, err := alice.DialContext(ctx, "", "")
			nc, err := alice.DialContext(ctx, "", a)
			if err != nil {
				t.Fatal(err)
			}

			EchoClient2(t, nc, nc)
			timeout := false
			timer := time.AfterFunc(600*time.Second, func() {
				timeout = true
				nc.Close()
			})

			nc.Write([]byte{0})
			log.Println("Request echo close write stream after", eh.Received)

			data := make([]byte, 1024)
			r, err := nc.Read(data)
			if err != io.EOF {
				t.Fatal("EOF not received")
			}
			log.Println("Read EOF ok: ", r, err)

			// The other direction should still be opened
			before := eh.Received
			nc.Write([]byte("PostClose"))
			time.Sleep(1 * time.Second)
			log.Println("Written after other direction close", eh.Received)
			if before+9 != eh.Received {
				t.Fatal("Write after close failed", before, eh.Received)
			}
			timer.Stop()
			if timeout {
				t.Fatal("Timeout waiting close")
			}
		}
	})

	// Evie opens hbone to TCP connection to bob's echo server.
	t.Run("invalid-root", func(t *testing.T) {
		evieca := auth.NewCA("cluster.local")

		evie := New(evieca.NewID("alice", "default"))
		evie.AddService(&Cluster{Addr: "default.bob:8080"}, &Endpoint{Address: bobEndpoint, HBoneAddress: bobHBAddr})

		_, err = evie.Dial("", "default.bob:8080")
		if err == nil {
			t.Fatal("Expecting error")
		}

	})

	t.Run("invalid-trust", func(t *testing.T) {
		evieca := auth.NewCA("notcluster.local")
		// Using the same root CA as bob/alice
		evieca.Private = ca.Private
		evieca.CACert = ca.CACert

		evie := New(evieca.NewID("alice", "default"))
		evie.AddService(&Cluster{Addr: "default.bob:8080"}, &Endpoint{Address: bobEndpoint, HBoneAddress: bobHBAddr})

		_, err = evie.Dial("", "default.bob:8080")
		if err == nil {
			t.Fatal("Expecting error")
		}

	})

	// Verify server first protocols work
	t.Run("plain-alice-bob-serverFirst", func(t *testing.T) {
		ehServerFirst := &echo.EchoHandler{ServerFirst: true, Debug: Debug}
		ehSFL, err := ehServerFirst.Start(":0")
		if err != nil {
			t.Fatal(err)
		}

		alice.AddService(&Cluster{Addr: "default.bob:6000"}, &Endpoint{Address: ehSFL.Addr().String(), HBoneAddress: bobHBAddr})

		nc, err := alice.Dial("", "default.bob:6000")
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 1024)
		n, err := nc.Read(b)
		if n == 0 || err != nil {
			t.Fatal(n, err)
		}
		EchoClient2(t, nc, nc)

		// Close client connection - expect FIN to be propagated to echo server, which will close it's out connection,
		// and we should receive io.EOF
		nc.Close()

		n, err = nc.Read(b)
		if err == nil {
			t.Fatal("Missing close")
		}
	})

	// ======== Gateway tests ============
	// SNI and H2R gate
	gateID := ca.NewID("gate", "default")
	gate := New(gateID)
	gateID.AllowedNamespaces = []string{"*"}

	gateH2, err := listenAndServeTCP(laddr(":14209"), gate.HandleAcceptedH2)
	if err != nil {
		t.Fatal(err)
	}

	// Old-style Istio SNI routing - should only be used on VPC, not exposed to internet
	gateSNIL, err := listenAndServeTCP(laddr(":14207"), func(conn net.Conn) {
		HandleSNIConn(gate, conn)
	})

	// "Reverse" connections (original, SNI based)
	gateH2RL, err := listenAndServeTCP(laddr(":14206"), gate.HandlerH2RConn)
	if err != nil {
		t.Fatal(err)
	}

	gate.AddService(&Cluster{Addr: "sni.bob.svc:443"},
		&Endpoint{
			Address:      bobEndpoint, // the echo server we want to reach
			HBoneAddress: bobHBAddr,
		})

	// WIP
	gate.AddService(&Cluster{Addr: "crun.bob.svc:8080"}, // k8s service addressing
		&Endpoint{
			Address:      "test.a.run.app:8080",                // the echo server we want to reach - port is local
			Labels:       map[string]string{"http_proxy": "1"}, // use POST, tokens
			HBoneAddress: "xxxx.a.run.app:443",
		})

	// 13022 is the SNI port of the gateway. It'll pass-through to the resolved address.
	alice.AddService(&Cluster{Addr: "sni.bob.svc:443"}, &Endpoint{
		Address: bobEndpoint, // the echo server we want to reach

		SNIGate: gateSNIL.Addr().String(),
		// Use the SNI gate as dest address
		HBoneAddress: gateSNIL.Addr().String(),
	})

	alice.AddService(&Cluster{Addr: "h2g.bob.svc:443"}, &Endpoint{
		Address:      bobEndpoint, // the echo server we want to reach
		HBoneAddress: gateH2.Addr().String(),
	})

	t.Run("sni-alice-gate-bob", func(t *testing.T) {
		nc, err := alice.Dial("", "sni.bob.svc:443")
		if err != nil {
			t.Fatal(err)
		}
		EchoClient2(t, nc, nc)
	})

	t.Run("sni-h2r-alice-gate-bob", func(t *testing.T) {

		// Connect bob to the gate.
		// ...

		//h2rc := bob.AddService(&Cluster{Addr: "gate.gate:13222"})

		// TODO: refactor h2r, use modified h2 stack
		// May have multiple h2r endpoints, to different instances (or all instances, if the gate is a stateful
		// set).
		h2re := RemoteForward(bob, gateH2RL.Addr().String(), "default", "bob")

		// Need to wait for the connection to show up - else the test is flaky
		// TODO: add a callback for 'h2r connection change', will be used to update
		// database.
		c, _ := gate.Cluster(ctx, "sni.bob.svc:443")
		for i := 0; i < 10; i++ {
			if len(c.EndpointCon) == 0 {
				time.Sleep(100 * time.Millisecond)
			} else {
				break
			}
		}

		//t.Run("ReverseProxySAN", func(t *testing.T) {
		//	rin, lout := io.Pipe()
		//	lin, rout := io.Pipe()
		//	go func() {
		//		c := alice.NewEndpointCon("https://" + gateH2RSNIL.Addr().String() + "/_hbone/tcp")
		//		c.SNI = "default.bob.svc.cluster.local"
		//		c.SNIGate = gateH2RSNIL.Addr().String()
		//
		//		err = c.Proxy(context.Background(), rin, rout)
		//		if err != nil {
		//			t.Fatal(err)
		//		}
		//	}()
		//
		//	EchoClient(t, lout, lin)
		//})

		/*		t.Run("ReverseProxyIstio", func(t *testing.T) {
					rin, lout := io.Pipe()
					lin, rout := io.Pipe()
					go func() {
						c := alice.NewEndpointCon("https://" + gateH2RSNIL.Addr().String() + "/_hbone/tcp")
						// The endpoint looks like an Istio endpoint.
						c.SNI = "outbound_.8080._.default.bob.svc.cluster.local"
						c.SNIGate = gateH2RSNIL.Addr().String()

						err = c.Proxy(context.Background(), rin, rout)
						if err != nil {
							t.Fatal(err)
						}
					}()

					EchoClient(t, lout, lin)
				})
		*/
		// Force close the tls con - server should terminate
		h2re.Close()

	})
}

func EchoClient2(t *testing.T, lout io.WriteCloser, lin io.Reader) {
	b := make([]byte, 1024)
	timer := time.AfterFunc(3*time.Second, func() {
		log.Println("timeout")
		//lin.CloseWithError(errors.New("timeout"))
		lout.Close() // (errors.New("timeout"))
	})
	lout.Write([]byte("Ping"))
	n, err := lin.Read(b)
	if n != 4 {
		t.Error(n, err)
	}
	timer.Stop()
}
