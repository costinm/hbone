package uxds

import (
	"context"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/costinm/hbone"
	"github.com/costinm/hbone/nio"
	meshca "github.com/costinm/hbone/urpc/gen/google/security/meshca/v1"
	istioca "github.com/costinm/hbone/urpc/gen/istio/v1/auth"
	auth "github.com/costinm/meshauth"
	"github.com/google/uuid"

	"github.com/costinm/hbone/urpc/gen/xds"
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
		c.ID = la.Name
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
	keyPEM, csr, err := id.NewCSR("spiffe://cluster.local/ns/default/sa/default")

	var ress *nio.Stream

	var res istioca.IstioCertificateResponse

	if strings.Contains(ca.Addr, "meshca.googleapis.com") {
		var req meshca.MeshCertificateRequest

		req.Csr = string(csr)
		req.RequestId = uuid.New().String()
		path := "/google.security.meshca.v1.MeshCertificateService/CreateCertificate"

		ress = NewGRPCStream(ctx, ca, ca.Addr, path)

		ress.Request.Header.Add("x-goog-request-params", "location=locations/us-central1-c")

		err = SendGPRC(ress, &req, &res)

	} else {
		// TODO: Add ClusterID header
		var req istioca.IstioCertificateRequest

		req.Csr = string(csr)
		path := "/istio.v1.auth.IstioCertificateService/CreateCertificate"

		ress = NewGRPCStream(ctx, ca, ca.Addr, path)

		err = SendGPRC(ress, &req, &res)
	}

	if err != nil {
		return err
	}

	err = id.SetKeysPEM(keyPEM, res.CertChain)
	return err
}
