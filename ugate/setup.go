package ugate

import (
	"net"

	"github.com/costinm/hbone/h2"
	"github.com/costinm/ugate"
	//otelprom "go.opentelemetry.io/otel/exporters/prometheus"
)

func Init(hc *ugate.UGate) {

	//ugate.H2TransportFactory = func(hb *ugate.UGate) {
	//	hb.H2Transport = &H2Transport{
	//		hb:      hb,
	//		H2RConn: map[*h2.H2Transport]*ugate.EndpointCon{},
	//	}
	//}
	hc.H2Transport = &H2Transport{
		hb:      hc,
		H2RConn: map[*h2.H2Transport]*ugate.EndpointCon{},
	}

	hc.Handlers["hbone"] = ugate.HandlerFunc(func(conn net.Conn) error {
		hc.HandleAcceptedH2(conn)
		return nil
	})
	hc.Handlers["hbonec"] = ugate.HandlerFunc(func(conn net.Conn) error {
		hc.HandleAcceptedH2C(conn)
		return nil
	})
}
