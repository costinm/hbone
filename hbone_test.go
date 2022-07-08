package hbone

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/costinm/hbone/auth"
	"github.com/costinm/hbone/echo"
)

func listenAndServeTCP(addr string, f func(conn net.Conn)) (net.Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	go nio.ServeListener(listener, f)
	return listener, nil
}

// WIP
func TestHBone(t *testing.T) {
	// New self-signed root CA
	ca := auth.NewCA("cluster.local")

	aliceID := ca.NewID("alice", "default")
	alice := New(aliceID)
	aliceID.AllowedNamespaces = []string{"*"}

	bobID := ca.NewID("bob", "default")
	bob := New(bobID)
	bobID.AllowedNamespaces = []string{"*"}
	l, err := listenAndServeTCP(":0", bob.HandleAcceptedH2)
	if err != nil {
		t.Fatal(err)
	}
	bobHBAddr := l.Addr().String()

	// Start an echo handler on bob
	eh := &echo.EchoHandler{Debug: Debug}
	ehL, err := eh.Start(":0")
	if err != nil {
		t.Fatal(err)
	}
	bob.TcpAddr = ehL.Addr().String()

	// Alice opens hbone to TCP connection to bob's echo server.
	t.Run("plain-alice-bob", func(t *testing.T) {
		rin, lout := io.Pipe()
		lin, rout := io.Pipe()
		go func() {
			err = alice.Proxy("default.bob:8080", "https://"+bobHBAddr+"/_hbone/tcp", rin, rout, nil)
			if err != nil {
				t.Fatal(err)
			}
		}()

		EchoClient(t, lout, lin)
	})

	t.Run("server-close", func(t *testing.T) {
		rin, lout := io.Pipe()
		lin, rout := io.Pipe()
		go func() {
			err = alice.Proxy("default.bob:8080", "https://"+bobHBAddr+"/_hbone/mtls", rin, rout, alice.Auth.GenerateTLSConfigServer())

			//err = alice.Proxy("default.bob:8080", "https://"+bobHBAddr+"/_hbone/tcp", rin, rout, nil)
			if err != nil {
				t.Fatal(err)
			}
		}()

		EchoClient(t, lout, lin)
		log.Println(eh.Received)
		lout.Write([]byte{0})
		log.Println(eh.Received)

		data := make([]byte, 1024)
		r, err := lin.Read(data)
		log.Println("Read: ", r, err)

		lout.Write([]byte("PostClose"))
		log.Println(eh.Received)
	})

	// Evie opens hbone to TCP connection to bob's echo server.
	t.Run("invalid-root", func(t *testing.T) {
		evieca := auth.NewCA("cluster.local")

		evie := New(evieca.NewID("alice", "default"))

		rin, _ := io.Pipe()
		_, rout := io.Pipe()
		err = evie.Proxy("default.bob:8080", "https://"+bobHBAddr+"/_hbone/tcp", rin, rout, nil)
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

		rin, _ := io.Pipe()
		_, rout := io.Pipe()
		err = evie.Proxy("default.bob:8080", "https://"+bobHBAddr+"/_hbone/tcp", rin, rout, nil)
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
		bob.Mux.HandleFunc("/_hbone/serverFirst", func(w http.ResponseWriter, r *http.Request) {
			err := bob.HandleTCPProxy(w, r.Body, ehSFL.Addr().String())
			log.Println("hbone serverFirst Proxy done ", r.RequestURI, err)
		})

		rin, lout := io.Pipe()
		lin, rout := io.Pipe()
		go func() {
			err = alice.Proxy("default.bob:6000", "https://"+bobHBAddr+"/_hbone/serverFirst", rin, rout, nil)
			if err != nil {
				t.Fatal(err)
			}
		}()
		b := make([]byte, 1024)
		n, err := lin.Read(b)
		if n == 0 || err != nil {
			t.Fatal(n, err)
		}
		EchoClient(t, lout, lin)

		// Close client connection - expect FIN to be propagated to echo server, which will close it's out connection,
		// and we should receive io.EOF
		lout.Close()

		n, err = lin.Read(b)
		if err == nil {
			t.Fatal("Missing close")
		}
	})

	t.Run("mtls-alice-bob", func(t *testing.T) {
		rin, lout := io.Pipe()
		lin, rout := io.Pipe()
		go func() {
			err = alice.Proxy("default.bob:8080", "https://"+bobHBAddr+"/_hbone/mtls", rin, rout, alice.Auth.GenerateTLSConfigServer())
			if err != nil {
				t.Fatal(err)
			}
		}()
		EchoClient(t, lout, lin)
	})

	// SNI and H2R gate
	gateID := ca.NewID("gate", "default")
	gate := New(gateID)
	gateID.AllowedNamespaces = []string{"*"}

	t.Run("sni-alice-gate-bob", func(t *testing.T) {
		gateSNIL, err := listenAndServeTCP(":0", func(conn net.Conn) {
			HandleSNIConn(gate, conn)
		})
		if err != nil {
			t.Fatal(err)
		}

		rin, lout := io.Pipe()
		lin, rout := io.Pipe()
		gate.EndpointResolver = func(sni string) *EndpointCon {
			gc := gate.NewEndpointCon("https://" + bobHBAddr + "/_hbone/mtls")

			return gc
		}

		go func() {
			// 13022 is the SNI port of the gateway. It'll pass-through to the resolved address.
			c := alice.NewEndpointCon("https://" + gateSNIL.Addr().String() + "/_hbone/tcp")
			c.SNI = "bob"
			c.SNIGate = gateSNIL.Addr().String()
			err = c.Proxy(context.Background(), rin, rout)
			if err != nil {
				t.Fatal(err)
			}
		}()

		EchoClient(t, lout, lin)
	})

	t.Run("sni-h2r-alice-gate-bob", func(t *testing.T) {
		gateH2RL, err := listenAndServeTCP(":0", gate.HandlerH2RConn)
		if err != nil {
			t.Fatal(err)
		}
		gateH2RSNIL, err := listenAndServeTCP(":0", gate.HandleH2RSNIConn)
		if err != nil {
			t.Fatal(err)
		}

		// Connect bob to the gate.
		// ...

		h2rc := bob.AddCluster("", &Cluster{Addr: "gate.gate:13222"})

		// May have multiple h2r endpoints, to different instances (or all instances, if the gate is a stateful
		// set).
		h2re := h2rc.NewEndpoint("")
		h2re.SNI = "default.bob.svc.cluster.local"

		h2rCtx, h2rCancel := context.WithCancel(context.Background())

		h2rCon, err := h2re.DialH2R(h2rCtx, gateH2RL.Addr().String())
		if err != nil {
			t.Fatal(err)
		}

		// Need to wait for the connection to show up - else the test is flaky
		// TODO: add a callback for 'h2r connection change', will be used to update
		// database.
		for i := 0; i < 10; i++ {
			if gate.H2R[h2re.SNI] == nil {
				time.Sleep(100 * time.Millisecond)
			} else {
				break
			}
		}

		t.Run("ReverseProxySAN", func(t *testing.T) {
			rin, lout := io.Pipe()
			lin, rout := io.Pipe()
			go func() {
				c := alice.NewEndpointCon("https://" + gateH2RSNIL.Addr().String() + "/_hbone/tcp")
				c.SNI = "default.bob.svc.cluster.local"
				c.SNIGate = gateH2RSNIL.Addr().String()

				err = c.Proxy(context.Background(), rin, rout)
				if err != nil {
					t.Fatal(err)
				}
			}()

			EchoClient(t, lout, lin)
		})

		t.Run("ReverseProxyIstio", func(t *testing.T) {
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

		// Force close the tls con - server should terminate
		h2rCon.Close()

		h2rCancel()
	})
}

func EchoClient(t *testing.T, lout *io.PipeWriter, lin *io.PipeReader) {
	b := make([]byte, 1024)
	timer := time.AfterFunc(3*time.Second, func() {
		log.Println("timeout")
		lin.CloseWithError(errors.New("timeout"))
		lout.CloseWithError(errors.New("timeout"))
	})
	lout.Write([]byte("Ping"))
	n, err := lin.Read(b)
	if n != 4 {
		t.Error(n, err)
	}
	timer.Stop()
}
