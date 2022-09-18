package uxds

import (
	"context"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/auth"
	istioca "github.com/costinm/hbone/ext/uxds/istio/v1/auth"

	"github.com/costinm/hbone/ext/uxds/xds"

	"google.golang.org/protobuf/proto"
)

// Example Istio: testdata/clusterloadassignment.json
func SetupControlPlane(ctx context.Context, hb *hbone.HBone, c *hbone.Cluster) error {
	xdsc, err := DialContext(ctx, "", &Config{
		Cluster: c,
		HBone:   hb,
	})
	if err != nil {
		return err
	}

	t0 := time.Now()
	for {
		res := <-xdsc.Updates
		if res == "eds" {
			break
		}
	}

	go func() {
		for {
			res := <-xdsc.Updates
			if res == "close" {
				log.Printf("XDS closed, re-dial", time.Since(t0))
				xdsc, err = DialContext(ctx, "", &Config{
					Cluster: c,
					HBone:   hb,
				})
				t0 = time.Now()
			}
		}
	}()

	return nil
}

func HandleCDS(hb *hbone.HBone, eds map[string]*xds.Cluster) {
	ctx := context.Background()
	for _, la := range eds {
		c, _ := hb.Cluster(ctx, la.Name)
		c.Id = la.Name
	}
}

func HandleEDS(hb *hbone.HBone, eds map[string]*xds.ClusterLoadAssignment) {
	ctx := context.Background()
	for _, la := range eds {
		// TODO: parse istio name, extract addr
		//
		c, _ := hb.Cluster(ctx, la.ClusterName)

		epc := []*hbone.Endpoint{}
		for _, lep := range la.Endpoints {
			// locality = region/zone/sub_zone
			// lbweight - 1..128
			// priority - 0 first, fallback to next bucket
			for _, ep := range lep.LbEndpoints {
				// lbweight - within the group
				// metadata - metadata_match, to subset, string->struct
				if epa := ep.Endpoint.Address.GetSocketAddress(); epa != nil {
					// protocol = TCP
					// address - IP or hostname to be resolved
					// resolver_name - how to resolve address, default to DNS
					// port - int or named port (not supported)
					addr := net.JoinHostPort(epa.Address, strconv.Itoa(int(epa.GetPortValue())))
					epc = append(epc, &hbone.Endpoint{Address: addr})
				}
				ep.LoadBalancingWeight = ep.LoadBalancingWeight
			}
		}

		// Basic mode - create an endpoint mux for each
		c.UpdateEndpoints(epc)
	}
}

// getCertificate is using Istio CA gRPC protocol to get a certificate for the id.
// Google implementation of the protocol is also supported.
func GetCertificate(ctx context.Context, id *auth.MeshAuth, ca *hbone.Cluster) error {
	// TODO: Add ClusterID header
	var req istioca.IstioCertificateRequest

	keyPEM, csr, err := id.NewCSR("spiffe://cluster.local/ns/default/sa/default")
	req.Csr = string(csr)

	// TODO: Add ClusterID header

	path := "/istio.v1.auth.IstioCertificateService/CreateCertificate"
	if strings.Contains(ca.Addr, "meshca.googleapis.com") {
		path = "/google.security.meshca.v1.MeshCertificateService/CreateCertificate"
	}

	ress := hbone.NewGRPCStream(ctx, ca, path)

	bb := ress.GetWriteFrame()
	bout, _ := proto.MarshalOptions{}.MarshalAppend(bb.Bytes(), &req)
	bb.UpdateAppend(bout)
	err = ress.Send(bb)
	ress.CloseWrite()

	if err != nil {
		return err
	}

	var res istioca.IstioCertificateResponse
	f, err := ress.Recv(true)
	proto.Unmarshal(f.Bytes(), &res)

	err = id.SetKeysPEM(keyPEM, res.CertChain)
	return err
}
