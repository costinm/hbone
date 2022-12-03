// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package grpcecho

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/h2"
	grpc "github.com/costinm/hbone/urpc"
	"github.com/costinm/hbone/urpc/gen/proto"
	"github.com/hashicorp/go-multierror"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc/metadata"
)

//var (
//	PortLabel = monitoring.MustCreateLabel("port")
//
//	GrpcRequests = monitoring.NewSum(
//		"istio_echo_grpc_requests_total",
//		"The number of grpc requests total",
//	)
//)
//
//func init() {
//	monitoring.MustRegister(Metrics.HTTPRequests, Metrics.GrpcRequests, Metrics.TCPRequests)
//}

const ECHO_SERVICE = "/proto.EchoTestService/Echo"

type EchoGrpcHandler struct {
	Port         int
	Version      string
	Cluster      string
	IstioVersion string

	Mesh *hbone.HBone
}

// Handle /grpc/ requests, equivalent with forward
func (h *EchoGrpcHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	s := writer.(*h2.H2Stream)
	urpc := grpc.NewFromStream(s)

	if strings.HasSuffix(request.URL.Path, "/Echo") {
		er := &proto.EchoRequest{}
		err := urpc.RecvMsg(er)
		if err != nil {
			return
		}
		res, err := h.Echo(s, s, er)
		if err != nil {
			return
		}
		urpc.SendMsg(res)
	} else {
		er := &proto.ForwardEchoRequest{}
		urpc.RecvMsg(er)
		res, err := h.ForwardEcho(s, er)
		if err != nil {
			return
		}
		urpc.SendMsg(res)
	}

	// TODO: set Trailers
	urpc.Stream.CloseWrite()
}

func (h *EchoGrpcHandler) Echo(ctx context.Context, s *h2.H2Stream, req *proto.EchoRequest) (*proto.EchoResponse, error) {
	// Using opencensus or otel or envoy telemetry
	//defer GrpcRequests.With(common.PortLabel.Value(strconv.Itoa(h.Port))).Increment()
	body := bytes.Buffer{}
	md := s.Request.Header
	for key, values := range md {
		if strings.HasSuffix(key, "-bin") {
			continue
		}
		field := Field(key)
		if key == ":authority" {
			field = HostField
		}
		for _, value := range values {
			writeField(&body, field, value)
		}
	}

	xfcc := md["x-forwarded-client-cert"]
	if xfcc != nil {
		// TODO: use authn package to extract original identity.
	}

	//id := uuid.New()
	//epLog.WithLabels("message", req.GetMessage(), "headers", md, "id", id).Infof("GRPC Request")

	portNumber := h.Port

	ip := "0.0.0.0"
	peerInfo := s.RemoteAddr()
	//if peerInfo, ok := peer.FromContext(ctx); ok {
	ip, _, _ = net.SplitHostPort(peerInfo.String())
	//}

	writeField(&body, StatusCodeField, StatusCodeOK)
	writeField(&body, ServiceVersionField, h.Version)
	writeField(&body, ServicePortField, strconv.Itoa(portNumber))
	writeField(&body, ClusterField, h.Cluster)
	writeField(&body, IPField, ip)
	writeField(&body, IstioVersionField, h.IstioVersion)
	writeField(&body, "Echo", req.GetMessage())

	if hostname, err := os.Hostname(); err == nil {
		writeField(&body, HostnameField, hostname)
	}

	//epLog.WithLabels("id", id).Infof("GRPC Response")
	return &proto.EchoResponse{Message: body.String()}, nil
}

const maxConcurrency = 20

var DefaultRequestTimeout = 5 * time.Second

func (h *EchoGrpcHandler) ForwardEcho(ctx context.Context, req *proto.ForwardEchoRequest) (*proto.ForwardEchoResponse, error) {
	g := multierror.Group{}
	responsesMu := sync.RWMutex{}
	responses, responseTimes := make([]string, req.Count), make([]time.Duration, req.Count)

	if req.Count == 0 {
		req.Count = 1
	}
	if req.TimeoutMicros == 0 {
		req.TimeoutMicros = DefaultRequestTimeout.Microseconds()
	}

	var throttle *time.Ticker

	if req.Qps > 0 {
		sleepTime := time.Second / time.Duration(req.Qps)
		//fwLog.Debugf("Sleeping %v between requests", sleepTime)
		throttle = time.NewTicker(sleepTime)
	}

	grpcConn, err := h.newClient(ctx, req)
	if err != nil {
		return nil, err
	}

	// make the timeout apply to the entire set of requests
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMicros)*time.Microsecond)
	var canceled bool
	defer func() {
		cancel()
		canceled = true
	}()

	sem := semaphore.NewWeighted(maxConcurrency)
	for reqIndex := 0; reqIndex < int(req.Count); reqIndex++ {
		rid := reqIndex

		if throttle != nil {
			<-throttle.C
		}

		if err := sem.Acquire(ctx, 1); err != nil {
			// this should only occur for a timeout, fallthrough to the ctx.Done() select case
			break
		}
		g.Go(func() error {
			defer sem.Release(1)
			if canceled {
				return fmt.Errorf("request set timed out")
			}
			st := time.Now()
			resp, err := h.makeRequest(ctx, grpcConn, req, rid)
			rt := time.Since(st)
			if err != nil {
				return err
			}
			responsesMu.Lock()
			responses[rid] = resp
			responseTimes[rid] = rt
			responsesMu.Unlock()
			return nil
		})
	}

	requestsDone := make(chan *multierror.Error)
	go func() {
		requestsDone <- g.Wait()
	}()

	select {
	case err := <-requestsDone:
		if err != nil {
			return nil, fmt.Errorf("%d/%d requests had errors; first error: %v", err.Len(), req.Count, err.Errors[0])
		}
	case <-ctx.Done():
		responsesMu.RLock()
		defer responsesMu.RUnlock()
		var c int
		var tt time.Duration
		for id, res := range responses {
			if res != "" && responseTimes[id] != 0 {
				c++
				tt += responseTimes[id]
			}
		}
		var avgTime time.Duration
		if c > 0 {
			avgTime = tt / time.Duration(c)
		}
		return nil, fmt.Errorf("request set timed out after %v and only %d/%d requests completed (%v avg)", req.TimeoutMicros, c, req.Count, avgTime)
	}

	return &proto.ForwardEchoResponse{
		Output: responses,
	}, nil
}

var (
	StatusCodeOK              = strconv.Itoa(http.StatusOK)
	StatusUnauthorized        = strconv.Itoa(http.StatusUnauthorized)
	StatusCodeForbidden       = strconv.Itoa(http.StatusForbidden)
	StatusCodeUnavailable     = strconv.Itoa(http.StatusServiceUnavailable)
	StatusCodeBadRequest      = strconv.Itoa(http.StatusBadRequest)
	StatusCodeTooManyRequests = strconv.Itoa(http.StatusTooManyRequests)
)

// Field is a list of fields returned in responses from the Echo server.
type Field string

const (
	RequestIDField      Field = "X-Request-Id"
	ServiceVersionField Field = "ServiceVersion"
	ServicePortField    Field = "ServicePort"
	StatusCodeField     Field = "StatusCode"
	URLField            Field = "URL"
	HostField           Field = "Host"
	HostnameField       Field = "Hostname"
	MethodField         Field = "Method"
	ResponseHeader      Field = "ResponseHeader"
	ClusterField        Field = "Cluster"
	IstioVersionField   Field = "IstioVersion"
	IPField             Field = "IP" // The Requester’s IP Address.
)

const (
	hostHeader = "Host"
)

const (
	ConnectionTimeout = 2 * time.Second
)

func (h *EchoGrpcHandler) newClient(ctx context.Context, req *proto.ForwardEchoRequest) (*grpc.UGRPC, error) {
	// NOTE: XDS load-balancing happens per-ForwardEchoRequest since we create a new client each time
	rawURL := req.Url
	var urlScheme string
	// grpc-go sets incorrect authority header
	if i := strings.IndexByte(rawURL, ':'); i > 0 {
		urlScheme = strings.ToLower(rawURL[0:i])
	}

	// Strip off the scheme from the address (for regular gRPC).
	address := rawURL
	if urlScheme == "grpc" {
		address = rawURL[len(urlScheme+"://"):]
	}

	// Connect to the GRPC server.
	ctx, cancel := context.WithTimeout(context.Background(), ConnectionTimeout)
	defer cancel()

	// This indicates using a different Host than in the address - for example address is an IP
	//host := ""
	//for _, k := range req.Headers {
	//	if k.Key == hostHeader {
	//		host = k.Value
	//	}
	//}
	c, err := h.Mesh.Cluster(ctx, address)
	grpcConn, err := grpc.New(ctx, c, address, "/proto.EchoTestService/Echo")
	if err != nil {
		return nil, err
	}
	return grpcConn, nil
}

func (h *EchoGrpcHandler) makeRequest(ctx context.Context, client *grpc.UGRPC, req *proto.ForwardEchoRequest, reqID int) (string, error) {

	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMicros)*time.Microsecond)
	defer cancel()

	// Add headers to the request context.
	outMD := make(metadata.MD)
	for _, v := range req.Headers {
		// Exclude the Host header from the GRPC context.
		if !strings.EqualFold(hostHeader, v.Key) {
			outMD.Set(v.Key, v.GetValue())
		}
	}
	outMD.Set("X-Request-Id", strconv.Itoa(reqID))
	ctx = metadata.NewOutgoingContext(ctx, outMD)

	var outBuffer bytes.Buffer
	grpcReq := &proto.EchoRequest{
		Message: req.Message,
	}
	outBuffer.WriteString(fmt.Sprintf("[%d] grpcecho.Echo(%v)\n", reqID, req))

	resp := &proto.EchoResponse{}
	err := client.Invoke(grpcReq, resp)
	if err != nil {
		return "", err
	}

	// when the underlying HTTP2 request returns status 404, GRPC
	// request does not return an error in grpc-go.
	// instead it just returns an empty response
	for _, line := range strings.Split(resp.GetMessage(), "\n") {
		if line != "" {
			outBuffer.WriteString(fmt.Sprintf("[%d body] %s\n", reqID, line))
		}
	}

	return outBuffer.String(), nil
}

// nolint: interfacer
func writeField(out *bytes.Buffer, field Field, value string) {
	_, _ = out.WriteString(string(field) + "=" + value + "\n")
}
