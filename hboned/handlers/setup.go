package handlers

import (
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/nio"
	"github.com/costinm/hbone/tel"

	//otelprom "go.opentelemetry.io/otel/exporters/prometheus"

	"sigs.k8s.io/yaml"
)

func InitXDSCluster(hb *hbone.HBone) *hbone.Cluster {
	// Control plane - must be configured. May setup the cert using Citadel or meshca
	xdsC := hb.GetCluster("istiod.istio-system.svc:15012")
	if xdsC == nil {
		istiodCA := hb.GetEnv("CAROOT_ISTIOD", "")
		if istiodCA == "" {
			istiodCA = hb.GetEnv("CAROOT", "")
		}
		istiodAddr := hb.GetEnv("MCON_ADDR", "")
		if istiodAddr == "" {
			istiodAddr = hb.GetEnv("XDS_ADDR", "")
		}
		if istiodAddr != "" {
			// Istiod cluster, using tokens
			xdsC = hb.AddService(&hbone.Cluster{
				Addr:        istiodAddr + ":15012",
				TokenSource: "istio-ca",
				ID:          "istiod.istio-system.svc:15012",
				SNI:         "istiod.istio-system.svc",
				CACert:      istiodCA,
			})
		}
	}

	return xdsC
}

func Start(hb *hbone.HBone) {
	hc := hb.MeshSettings
	//id := hb.ID
	var err error
	InitMDS(hb)

	InitExpvar(hb)

	// TODO: replace with a label on clusters that need to maintain persistent connections.
	// XDS is an example.
	// H2R will be available in all clusters that support the Setting.
	//// Create a reverse tunnel, also making this node accessible from the mesh
	//h2r := hb.GetCluster("h2r")
	//if h2r != nil {
	//	go RemoteForward(hb, h2r.Addr, id.Namespace, id.Name)
	//}

	// Key is port (string), value is protocol or forward address
	var hbonePort *hbone.Listener
	for lname, l := range hc.Listeners {
		if l.Address == "" {
			l.Address = lname
		}
		port := l.Address
		v := l.Protocol

		pn, _ := strconv.Atoi(port)

		switch v {
		case "sni": // 15003
			listenServe(l, port, func(conn net.Conn) {
				HandleSNIConn(hb, conn)
			})
		case "socks": // should be on 1080 for default
			listenServe(l, "127.0.0.1:"+port, func(conn net.Conn) {
				err = HandleSocksConn(hb, conn)
				if err != nil {
					log.Println("Error handling SOCKS", err)
				}
			})
		case "hbone": // 15008
			hbonePort = l
		case "hbonec": // 15009
			listenServe(l, port, hb.HandleAcceptedH2C)
		case "admin": // 15000
			go func() {
				err = http.ListenAndServe(port, nil)
				if err != nil {
					log.Fatal(err)
				}
			}()
		case "metrics": // 15020
			go func() {
				err = http.ListenAndServe("0.0.0.0:"+port, http.HandlerFunc(tel.HandleMetrics))
				if err != nil {
					log.Fatal(err)
				}
			}()
		case "tproxy":
			utp, _ := nio.StartUDPTProxyListener6(pn)
			if utp != nil {
				nio.UDPAccept(utp, hb.HandleUdp)
			}
			nio.IptablesCapture(":"+port, hb.HandleTUN)
		default:
			hf1 := hb.Handlers[l.Protocol]
			if hf1 == nil {
				log.Println("Unspecified port, default to forward (in)")
			} else {
				listenServe(l, port, func(conn net.Conn) {
					hf1.HandleConn(conn)
				})
			}
		}
	}

	for p, a := range hc.LocalForward {
		port := p
		addr := a
		go func() {
			err := hbone.LocalForwardPort(fmt.Sprintf("127.0.0.1:%d", port), addr, hb)
			if err != nil {
				log.Fatal("Failed to forward port", err)
			}
		}()
	}

	// Last - the HBone port
	if hbonePort == nil {
		hbonePort = &hbone.Listener{
			Address: "15008",
		}
	}
	listenServe(hbonePort, hbonePort.Address, hb.HandleAcceptedH2)
}

func listenServe(listener *hbone.Listener, port string, f func(net.Conn)) {
	if port != "-" && port != "" {
		ll, err := nio.ListenAndServe(port, f)
		listener.NetListener = ll
		if err != nil {
			log.Fatal("Failed to listen on ", port, err)
		}
	}
}

func LoadMeshConfig(hc *hbone.MeshSettings, path string) error {
	var cfg []byte
	var err error

	cfgEnv := os.Getenv("HBONE_CFG_YAML")
	if cfgEnv != "" {
		cfg = []byte(cfgEnv)
	} else {
		if path == "" {
			path = os.Getenv("HBONE_CFG")
		}
		if path == "" {
			path = "hbone.yaml"
		}
		if _, err := os.Stat(path); err == nil {
			cfg, err = ioutil.ReadFile(path)
			if err != nil {
				return err
			}
		}
	}

	err = yaml.Unmarshal(cfg, hc)
	if err != nil {
		log.Fatal("Failed to decode config", err)
	}

	// TODO: process env variables
	for _, kvs := range os.Environ() {
		kv := strings.SplitN(kvs, "=", 2)
		k := kv[0]
		switch k {
		case "NAMESPACE":
			hc.Namespace = kv[1]
		}

		if strings.HasPrefix(k, "PORT_") {
			hc.Ports[k[5:]] = kv[1]
		}
	}

	return err
}
