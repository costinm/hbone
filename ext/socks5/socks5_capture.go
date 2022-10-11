package socks5

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/nio"
)

// Egress capture using SOCKS5, for whitebox mode.

// curl --socks5 127.0.0.1:15004 ....
// export HTTP_PROXY=socks5://127.0.0.1:15004

// Note: max DNS size is 255 ( including trailing 0, and len labels )

const (
	ConnectCommand   = uint8(1)
	BindCommand      = uint8(2)
	AssociateCommand = uint8(3)
	ipv4Address      = uint8(1)
	fqdnAddress      = uint8(3)
	ipv6Address      = uint8(4)
)

const (
	successReply uint8 = iota
	serverFailure
	ruleFailure
	networkUnreachable
	hostUnreachable
	connectionRefused
	ttlExpired
	commandNotSupported
	addrTypeNotSupported
)

const (
	NoAuth          = uint8(0)
	noAcceptable    = uint8(255)
	UserPassAuth    = uint8(2)
	userAuthVersion = uint8(1)
	authSuccess     = uint8(0)
	authFailure     = uint8(1)
)

/*
  RFC1928

  1. Req:
  VER 0x05
  NMETHODS 0x01
  METHOD 0x00 [one byte for each method - NoAuth]
  (other auth not supported - we bind on 127.0.0.1 or use mtls)

  Res:
  VER 0x05
	METHOD 0x00

	2.  VER: X'05'
      CMD
             o  CONNECT X'01'
             o  BIND X'02'
             o  UDP ASSOCIATE X'03'
      RSV    RESERVED 0x00
      ATYP   address type of following address
             o  IP V4 address: X'01'
             o  DOMAINNAME: X'03'
             o  IP V6 address: X'04'
      DST.ADDR       desired destination address
      DST.PORT desired destination port in network octet order
*/

type Socks struct {
	Dest     string
	DestAddr *net.TCPAddr
	w        io.WriteCloser
}

// Must be called before sending any data.
func (s *Socks) PostDialHandler(conn net.Conn, err error) {
	if err != nil {
		// TODO: write error code
		s.w.Write([]byte{5, 1})
		s.w.Close()
		return
	}
	// Not accurate for tcp-over-http.
	// TODO: pass a 'on connect' callback

	localAddr := conn.LocalAddr()
	tcpAddr := localAddr.(*net.TCPAddr)
	r := make([]byte, len(tcpAddr.IP)+6)
	r[0] = 5
	r[1] = 0 // success
	r[2] = 0 // rsv
	off := 4
	if tcpAddr.IP.To4() != nil {
		r[3] = 1
		copy(r[off:off+4], []byte(tcpAddr.IP))
		off += 4
	} else {
		r[3] = 2
		copy(r[off:off+16], []byte(tcpAddr.IP))
		off += 16
	}
	binary.BigEndian.PutUint16(r[off:], uint16(tcpAddr.Port))
	off += 2
	s.w.Write(r[0:off])
}

func HandleSocksConn(hb *hbone.HBone, conn net.Conn) error {
	brin := nio.NewBufferReader(conn)
	s := &Socks{w: conn}
	err := HandleSocks(brin, s, conn)
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

func HandleSocks(br *nio.Buffer, s *Socks, w io.WriteCloser) (err error) {
	// Fill the read buffer with one Read.
	// Typically 3-4 bytes unless client is eager.

	head, err := br.Peek(3)
	if err != nil {
		return err
	}

	if head[0] != 5 {
		return errors.New("invalid header")
	}
	// Client: 0x05 0x01 0x00
	//         0x05 0x02  0x00 0x01
	// Server: 0x05 0x00
	off := 1
	sz := int(head[off])
	off++                   // 2
	if len(head) < off+sz { // if it only read 2, probably malicious - 2 < 2 + 1
		head, err = br.Peek(off + sz)
		if err != nil {
			return err
		}
	}
	off += sz // 3

	w.Write([]byte{5, 0})

	// We may have bytes in the buffer, in case sender didn't wait
	if len(head) <= off+6 {
		head, err = br.Peek(off + sz)
		if err != nil {
			return err
		}
	}
	// We have at least 6 bytes
	if head[off] != 5 {
		return errors.New("invalid header 2")
	}
	off++
	if head[off] != 1 {
		return errors.New("invalid method " + strconv.Itoa(int(head[off])))
	}
	off++
	off++ // rsvd

	atyp := head[off]
	off++

	destName := ""
	var destIP []byte
	// off should be 3 or 4
	switch atyp {
	case 1:
		if len(head) <= off+6 {
			head, err = br.Peek(off + 6)
		}
		destIP = make([]byte, 4)
		copy(destIP, head[off:off+4])
		off += 4
	case 4:
		if len(head) <= off+18 {
			head, err = br.Peek(off + 18)
		}
		destIP = make([]byte, 16)
		copy(destIP, head[off:off+16])
		off += 16

	case 3:
		dlen := int(head[off])
		off++
		if len(head) <= off+dlen+2 {
			head, err = br.Peek(off + dlen + 2)
		}
		destName = string(head[off : off+dlen])
		off += dlen
	}
	if err != nil {
		return err
	}
	port := binary.BigEndian.Uint16(head[off:])
	off += 2

	// Any reminding bytes are eager sent
	br.Discard(off)

	if atyp == 3 {
		s.Dest = net.JoinHostPort(destName, strconv.Itoa(int(port)))
	} else {
		s.DestAddr = &net.TCPAddr{IP: destIP, Port: int(port)}
		s.Dest = s.DestAddr.String()
	}

	return nil
}
