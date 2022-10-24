package nio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
)

// RoundTripStart() must be called for clients to initiate the roundtrip.
// It will not block - client must start writing to the stream, will get back
// the headers with the first response bytes.
//
// Not part of NewGRPCStream because client may set additional headers.
func (hc *Stream) RoundTripStart() {
	if hc.rtCh != nil || hc.Response != nil {
		return // already started
	}
	hc.rtCh = make(chan error)
	// Needs to be in a go routine - Write is blocking until bytes are
	// sent, which happens during roundtrip.
	go func() {
		// TODO: RoundTripStart to find the cluster.

		hres, err := hc.Cluster.RoundTrip(hc.Request)
		if err == nil {
			hc.Response = hres
			hc.In = hres.Body

			if hres.StatusCode >= 300 || hres.StatusCode < 200 {
				err = errors.New(fmt.Sprintf("status code %v",
					hres.StatusCode))
			}
			hc.rbuffer = NewBuffer()
		}

		if err != nil {
			if c, ok := hc.Out.(io.Closer); ok {
				c.Close()
			}
		}

		if hc.ErrChan != nil {
			hc.ErrChan <- err
		}

		hc.rtCh <- err
	}()
}

//func (hc *Stream) Close() error {
//	// TODO: send trailers if server !!!
//	//hc.W.Close()
//	if cw, ok := hc.Out.(io.Closer); ok {
//		cw.Close()
//	}
//	if cw, ok := hc.In.(io.Closer); ok {
//		return cw.Close()
//	}
//
//	return nil
//}

func (hc *Stream) Recv(last bool) (*Buffer, error) {
	if hc.Response != nil {
		if hc.Response.StatusCode >= 300 || hc.Response.StatusCode < 200 {
			return nil, errors.New(fmt.Sprintf("status code %v",
				hc.Response.StatusCode))
		}
	}

	if hc.RcvdPackets == 0 {
		err := <-hc.rtCh
		if err != nil {
			return nil, err
		}
	}

	b := hc.rbuffer

	if hc.RcvdPackets > 0 {
		b.Discard(int(5 + len(hc.lastFrame.Bytes())))
	} else {
		//log.Println("Header: ", hc.Res.Header)
		// Initial headers don't include grpc-status - just 200
		// grpc-encoding - compression

	}

	// TODO: only if an incomplete frame.
	if !b.IsEmpty() {
		b.Compact()
	}

	hc.RcvdPackets++

	var mlen uint32
	head, err := b.Fill(hc.In, 5)
	if err == nil {
		if head[0] != 0 {
			return nil, fmt.Errorf("Invalid frame %d", head[0])
		}
		mlen = binary.BigEndian.Uint32(head[1:])

		_, err = b.Fill(hc.In, int(5+mlen))
	}
	// At this point, Res should be set.

	if err == io.EOF {
		// TODO: extract, see http2-client in grpc
		// grpc-status
		// grpc-message
		// grpc-status-details-bin - base64 proto
		if hc.Response != nil {
			log.Println("Trailer", hc.Response.Trailer)
			hc.Response.Body.Close()
		}
	} else if err != nil {
		return nil, err
	}

	if hc.Response.Trailer.Get("") != "" {

	}

	hc.lastFrame = b.Frame(5, int(5+mlen))
	return hc.lastFrame, err
}

// Return a buffer with reserved front space to be used for appending.
// If using functions like proto.Marshal, b.UpdateForAppend should be called
// with the new []byte. App should not touch the prefix.
func (hc *Stream) GetWriteFrame() *Buffer {
	//if m.OutFrame != nil {
	//	return m.OutFrame
	//}
	b := GetBuffer()
	b.WriteByte(0)
	b.WriteUnint32(0)
	return b
}

// Framed sending/receiving.
func (hc *Stream) Send(b *Buffer) error {

	hc.SentPackets++
	frameLen := b.Size() - 5
	binary.BigEndian.PutUint32(b.Bytes()[1:], uint32(frameLen))

	_, err := hc.Out.Write(b.Bytes())
	if f, ok := hc.Out.(http.Flusher); ok {
		f.Flush()
	}

	b.Recycle()

	return err
}
