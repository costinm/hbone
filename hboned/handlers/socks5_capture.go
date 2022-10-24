package handlers

import (
	"net"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/nio"
)

func HandleSocksConn(hb *hbone.HBone, conn net.Conn) error {
	brin := nio.NewBufferReader(conn)

	s := &nio.Socks{Writer: conn}
	err := nio.HandleSocks(brin, s, conn)
	if err != nil {
		return err
	}

	// s.Dest is now populated. Depending on client, s.DestAddr may be populated too.
	nc, err := hb.Dial("tcp", s.Dest)
	if err != nil {
		s.PostDialHandler(nil, err)
		return err
	}
	s.PostDialHandler(nc, nil)

	return hbone.Proxy(nc, brin, conn, s.Dest)
}
