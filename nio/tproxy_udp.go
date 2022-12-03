package nio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/net/ipv4"
)

// WIP: requires root, UDP proxy using orig dest.

// To preserve the original srcIP it is also possible to use udp.WriteMsg.

func UDPAccept(con *net.UDPConn, u func(ip net.IP, port uint16, ip2 net.IP, u uint16, bytes []byte)) {
	//go tu.blockingLoop(tu.con, u)
	go blockingLoop(con, u)
}

// ReadFromUDP reads a UDP packet from c, copying the payload into b.
// It returns the number of bytes copied into b and the return address
// that was on the packet.
//
// Out-of-band data is also read in so that the original destination
// address can be identified and parsed.
func ReadFromUDP(conn *net.UDPConn, b []byte) (int, *net.UDPAddr, *net.UDPAddr, error) {
	oob := make([]byte, 2048)
	n, oobn, _, addr, err := conn.ReadMsgUDP(b, oob)
	if err != nil {
		return 0, nil, nil, err
	}

	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return 0, nil, nil, fmt.Errorf("parsing socket control message: %s", err)
	}

	var originalDst *net.UDPAddr
	for _, msg := range msgs {
		//if msg.Header.Level == syscall.SOL_IPV6 && msg.Header.Type == syscall.IPV6_PKTINFO {
		//	// 20 bytes
		//	log.Println(msg.Header.Level, msg.Header.Type, msg.Data)
		//	rawaddr := make([]byte, 16)
		//	// raw IP address, 4 bytes for IPv4 or 16 bytes for IPv6, only IPv4 is supported now
		//	copy(rawaddr, msg.Data[0:16])
		//	origIP := net.IP(rawaddr)
		//
		//	// Bigendian is the network bit order, seems to be used here.
		//	origPort := binary.BigEndian.Uint16(msg.Data[16:])
		//	originalDst = &net.UDPAddr{
		//		IP:   origIP,
		//		Port: int(origPort),
		//	}
		//} else
		if msg.Header.Level == syscall.SOL_IPV6 && msg.Header.Type == 74 {
			// 28 bytes - family, port,
			rawaddr := make([]byte, 16)
			// raw IP address, 4 bytes for IPv4 or 16 bytes for IPv6, only IPv4 is supported now
			copy(rawaddr, msg.Data[8:])
			origIP := net.IP(rawaddr)

			// Bigendian is the network bit order, seems to be used here.
			origPort := binary.BigEndian.Uint16(msg.Data[2:])
			originalDst = &net.UDPAddr{
				IP:   origIP,
				Port: int(origPort),
			}
		} else if msg.Header.Level == syscall.SOL_IP && msg.Header.Type == syscall.IP_RECVORIGDSTADDR {
			originalDstRaw := &syscall.RawSockaddrInet4{}
			if err = binary.Read(bytes.NewReader(msg.Data), binary.LittleEndian, originalDstRaw); err != nil {
				return 0, nil, nil, fmt.Errorf("reading original destination address: %s", err)
			}

			switch originalDstRaw.Family {
			case syscall.AF_INET:
				pp := (*syscall.RawSockaddrInet4)(unsafe.Pointer(originalDstRaw))
				p := (*[2]byte)(unsafe.Pointer(&pp.Port))
				originalDst = &net.UDPAddr{
					IP:   net.IPv4(pp.Addr[0], pp.Addr[1], pp.Addr[2], pp.Addr[3]),
					Port: int(p[0])<<8 + int(p[1]),
				}

			case syscall.AF_INET6:
				pp := (*syscall.RawSockaddrInet6)(unsafe.Pointer(originalDstRaw))
				p := (*[2]byte)(unsafe.Pointer(&pp.Port))
				originalDst = &net.UDPAddr{
					IP:   net.IP(pp.Addr[:]),
					Port: int(p[0])<<8 + int(p[1]),
					Zone: strconv.Itoa(int(pp.Scope_id)),
				}

			default:
				return 0, nil, nil, fmt.Errorf("original destination is an unsupported network family")
			}
		} else {
			log.Println(msg.Header.Level, msg.Header.Type)
		}
	}

	if originalDst == nil {
		return 0, nil, nil, fmt.Errorf("unable to obtain original destination: %s", err)
	}

	return n, addr, originalDst, nil
}

// Handle packets received on the tproxy interface.
func blockingLoop(con *net.UDPConn, u func(ip net.IP, port uint16, ip2 net.IP, u uint16, bytes []byte)) {
	data := make([]byte, 1600)

	for {
		n, a1, addr, err := ReadFromUDP(con, data)
		if err != nil {
			log.Println("Read err", err)
			continue
		}

		go u(a1.IP, uint16(a1.Port), addr.IP, uint16(addr.Port), data[0:n])
	}
}

// Initialize a port as a TPROXY socket. This can be sent over UDS from the root, and used for
// UDP capture.
func StartUDPTProxyListener6(port int) (*net.UDPConn, error) {
	network := "udp"
	laddr := &net.UDPAddr{Port: port}
	listener, err := net.ListenUDP(network, laddr)
	if err != nil {
		return nil, err
	}

	fileDescriptorSource, err := listener.File()
	if err != nil {
		return nil, &net.OpError{Op: "listen", Net: network, Source: nil, Addr: laddr, Err: fmt.Errorf("get file descriptor: %s", err)}
	}
	defer fileDescriptorSource.Close()

	fileDescriptor := int(fileDescriptorSource.Fd())
	if err = syscall.SetsockoptInt(fileDescriptor, syscall.SOL_IP, syscall.IP_TRANSPARENT, 1); err != nil {
		return nil, &net.OpError{Op: "listen", Net: network, Source: nil, Addr: laddr, Err: fmt.Errorf("set socket option: IP_TRANSPARENT: %s", err)}
	}

	if err = syscall.SetsockoptInt(fileDescriptor, syscall.IPPROTO_IP, syscall.IP_RECVORIGDSTADDR, 1); err != nil {
		return nil, &net.OpError{Op: "listen", Net: network, Source: nil, Addr: laddr, Err: fmt.Errorf("set socket option: IP_RECVORIGDSTADDR: %s", err)}
	}
	// doesn't work
	//	if err = syscall.SetsockoptInt(fileDescriptor, syscall.IPPROTO_IPV6, syscall.IP_RECVORIGDSTADDR, 1); err != nil {
	//		return nil, &net.OpError{Op: "listen", Net: network, Source: nil, Addr: laddr, Err: fmt.Errorf("set socket option: IP_RECVORIGDSTADDR: %s", err)}
	//	}
	// only ip
	//if err = syscall.SetsockoptInt(fileDescriptor, syscall.IPPROTO_IPV6, syscall.IPV6_RECVPKTINFO, 1); err != nil {
	//	return nil, &net.OpError{Op: "listen", Net: network, Source: nil, Addr: laddr, Err: fmt.Errorf("set socket option: IP_RECVORIGDSTADDR: %s", err)}
	//}
	// IPV6_ORIGDSTADDR
	if err = syscall.SetsockoptInt(fileDescriptor, syscall.IPPROTO_IPV6, 74, 1); err != nil {
		return nil, &net.OpError{Op: "listen", Net: network, Source: nil, Addr: laddr, Err: fmt.Errorf("set socket option: IP_RECVORIGDSTADDR: %s", err)}
	}
	// no work
	//if err = syscall.SetsockoptInt(fileDescriptor, syscall.IPPROTO_IPV6, syscall.IPV6_DSTOPTS, 1); err != nil {
	//	return nil, &net.OpError{Op: "listen", Net: network, Source: nil, Addr: laddr, Err: fmt.Errorf("set socket option: IP_RECVORIGDSTADDR: %s", err)}
	//}
	// New
	//if err = syscall.SetsockoptInt(fileDescriptor, syscall.IPPROTO_IP, syscall.IP_PKTINFO, 1); err != nil {
	//	return nil, &net.OpError{Op: "listen", Net: network, Source: nil, Addr: laddr, Err: fmt.Errorf("set socket option: IP_RECVORIGDSTADDR: %s", err)}
	//}

	return listener, nil
}

// Handles UDP packets intercepted using TProxy.
// Can send packets preserving original IP/port.
type TProxyUDP struct {
	con  *net.UDPConn
	con6 *net.UDPConn
}

// UDP write with source address control.
func (tudp *TProxyUDP) WriteTo(data []byte, dstAddr *net.UDPAddr, srcAddr *net.UDPAddr) (int, error) {

	// Attempt to write as UDP
	cm4 := new(ipv4.ControlMessage)
	cm4.Src = srcAddr.IP
	oob := cm4.Marshal()
	n, _, err := tudp.con.WriteMsgUDP(data, oob, dstAddr)
	if err != nil {
		n, err = tudp.con.WriteToUDP(data, dstAddr)
		if err != nil {
			log.Print("Failed to send DNS ", dstAddr, srcAddr)
		}
	}

	return n, err // tudp.con.WriteTo(data, dstAddr)
}
