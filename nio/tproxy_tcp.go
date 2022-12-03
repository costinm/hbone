package nio

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// Based on https://github.com/LiamHaworth/go-tproxy/blob/master/tproxy_tcp.go

// AcceptTProxy will accept a TCP connection
// and wrap it to a TProxy connection to provide
// TProxy functionality
func AcceptTProxy(l *net.TCPListener) (*net.TCPConn, error) {
	tcpConn, err := l.AcceptTCP()
	if err != nil {
		return nil, err
	}

	return tcpConn, nil
}

// ListenTProxy will construct a new TCP listener
// socket with the Linux IP_TRANSPARENT option
// set on the underlying socket
func ListenTProxy(network string, laddr *net.TCPAddr) (*net.TCPListener, error) {
	listener, err := net.ListenTCP(network, laddr)
	if err != nil {
		return nil, err
	}

	fileDescriptorSource, err := listener.File()
	if err != nil {
		return nil, &net.OpError{Op: "listen", Net: network, Source: nil, Addr: laddr, Err: fmt.Errorf("get file descriptor: %s", err)}
	}
	defer fileDescriptorSource.Close()

	if err = syscall.SetsockoptInt(int(fileDescriptorSource.Fd()), syscall.SOL_IP, syscall.IP_TRANSPARENT, 1); err != nil {
		return nil, &net.OpError{Op: "listen", Net: network, Source: nil, Addr: laddr, Err: fmt.Errorf("set socket option: IP_TRANSPARENT: %s", err)}
	}

	return listener, nil
}

func IptablesCapture(addr string, ug func(nc net.Conn, dest, la *net.TCPAddr)) error {
	na, err := net.ResolveTCPAddr("tcp", addr)
	nl, err := ListenTProxy("tcp", na)
	if err != nil {
		log.Println("Failed to listen", err)
		return err
	}
	for {
		remoteConn, err := AcceptTProxy(nl)
		//ugate.VarzAccepted.Add(1)
		if ne, ok := err.(net.Error); ok {
			//ugate.VarzAcceptErr.Add(1)
			if ne.Temporary() {
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}
		if err != nil {
			log.Println("Accept error, closing iptables listener ", err)
			return err
		}

		ug(remoteConn, remoteConn.LocalAddr().(*net.TCPAddr), remoteConn.RemoteAddr().(*net.TCPAddr))
	}

	return nil
}

// Status:
//   https://upload.wikimedia.org/wikipedia/commons/3/37/Netfilter-packet-flow.svg
//
// Using: https://github.com/Snawoot/transocks/blob/v1.0.0/original_dst_linux.go
// ServeConn is used to serve a single TCP UdpNat.
// See https://github.com/cybozu-go/transocks
// https://github.com/ryanchapman/go-any-proxy/blob/master/any_proxy.go,
// and other examples.
// Based on REDIRECT.

const (
	SO_ORIGINAL_DST      = 80
	IP6T_SO_ORIGINAL_DST = 80
)

func getsockopt(s int, level int, optname int, optval unsafe.Pointer, optlen *uint32) (err error) {
	_, _, e := syscall.Syscall6(
		syscall.SYS_GETSOCKOPT, uintptr(s), uintptr(level), uintptr(optname),
		uintptr(optval), uintptr(unsafe.Pointer(optlen)), 0)
	if e != 0 {
		return e
	}
	return
}

// Should be used only for REDIRECT capture.
func GetREDIRECTOriginalDst(clientConn *net.TCPConn) (rawaddr *net.TCPAddr, err error) {
	// test if the underlying fd is nil
	remoteAddr := clientConn.RemoteAddr()
	if remoteAddr == nil {
		err = errors.New("fd is nil")
		return
	}

	// net.TCPConn.File() will cause the receiver's (clientConn) socket to be placed in blocking mode.
	// The workaround is to take the File returned by .File(), do getsockopt() to get the original
	// destination, then create a new *net.TCPConn by calling net.Stream.FileConn().  The new TCPConn
	// will be in non-blocking mode.  What a pain.
	clientConnFile, err := clientConn.File()
	if err != nil {
		return
	}
	defer clientConnFile.Close()

	fd := int(clientConnFile.Fd())
	if err = syscall.SetNonblock(fd, true); err != nil {
		return
	}

	// Get original destination
	// this is the only syscall in the Golang libs that I can find that returns 16 bytes
	// Example result: &{Multiaddr:[2 0 31 144 206 190 36 45 0 0 0 0 0 0 0 0] Interface:0}
	// port starts at the 3rd byte and is 2 bytes long (31 144 = port 8080)
	// IPv6 version, didn't find a way to detect network family
	//addr, err := syscall.GetsockoptIPv6Mreq(int(clientConnFile.Fd()), syscall.IPPROTO_IPV6, IP6T_SO_ORIGINAL_DST)
	// IPv4 address starts at the 5th byte, 4 bytes long (206 190 36 45)
	v6 := clientConn.LocalAddr().(*net.TCPAddr).IP.To4() == nil
	if v6 {
		var addr syscall.RawSockaddrInet6
		var len uint32
		len = uint32(unsafe.Sizeof(addr))
		err = getsockopt(fd, syscall.IPPROTO_IPV6, IP6T_SO_ORIGINAL_DST,
			unsafe.Pointer(&addr), &len)
		if err != nil {
			return
		}
		ip := make([]byte, 16)
		for i, b := range addr.Addr {
			ip[i] = b
		}
		pb := *(*[2]byte)(unsafe.Pointer(&addr.Port))
		return &net.TCPAddr{
			IP:   ip,
			Port: int(pb[0])*256 + int(pb[1]),
		}, nil
	} else {
		var addr syscall.RawSockaddrInet4
		var len uint32
		len = uint32(unsafe.Sizeof(addr))
		err = getsockopt(fd, syscall.IPPROTO_IP, SO_ORIGINAL_DST,
			unsafe.Pointer(&addr), &len)
		if err != nil {
			return nil, os.NewSyscallError("getsockopt", err)
		}
		ip := make([]byte, 4)
		for i, b := range addr.Addr {
			ip[i] = b
		}
		pb := *(*[2]byte)(unsafe.Pointer(&addr.Port))
		return &net.TCPAddr{
			IP:   ip,
			Port: int(pb[0])*256 + int(pb[1]),
		}, nil
	}
}

//func isLittleEndian() bool {
//	var i int32 = 0x01020304
//	u := unsafe.Pointer(&i)
//	pb := (*byte)(u)
//	b := *pb
//	return (b == 0x04)
//}

//var (
//	NativeOrder binary.ByteOrder
//)
//
//func init() {
//	if isLittleEndian() {
//		NativeOrder = binary.LittleEndian
//	} else {
//		NativeOrder = binary.BigEndian
//	}
//}
