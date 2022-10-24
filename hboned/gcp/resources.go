package gcp

// Extracted minimal resources to reduce deps.

// Subset of the generated GCP config - to avoid a dependency and bloat.
// This include just what we need for bootstraping from GKE, Hub or secret store.

// Clusters return the list of GKE clusters.
type Clusters struct {
	Clusters []*Cluster
}

type Cluster struct {
	Name string

	// nodeConfig
	MasterAuth struct {
		ClusterCaCertificate []byte
	}
	Location string

	Endpoint string

	ResourceLabels map[string]string

	// Extras:

	// loggingService, monitoringService
	//Network string "default"
	//Subnetwork string
	ClusterIpv4Cidr  string
	ServicesIpv4Cidr string
	// addonsConfig
	// nodePools

	// For regional clusters - each zone.
	// For zonal - one entry, equal with location
	Locations []string
	// ipAllocationPolicy - clusterIpv4Cider, serviceIpv4Cider...
	// masterAuthorizedNetworksConfig
	// maintenancePolicy
	// autoscaling
	NetworkConfig struct {
		// projects/NAME/global/networks/default
		Network    string
		Subnetwork string
	}
	// releaseChannel
	// workloadIdentityConfig

	// It seems zone and location are the same for zonal clusters.
	//Zone string // ex: us-west1
}

// HubClusters return the list of clusters registered in GKE Hub.
type HubClusters struct {
	Resources []HubCluster
}

type HubCluster struct {
	// Full name - projects/wlhe-cr/locations/global/memberships/asm-cr
	//Name     string
	Endpoint *struct {
		GkeCluster *struct {
			// //container.googleapis.com/projects/wlhe-cr/locations/us-central1-c/clusters/asm-cr
			ResourceLink string
		}
		// kubernetesMetadata: vcpuCount, nodeCount, api version
	}
	State *struct {
		// READY
		Code string
	}

	Authority struct {
		Issuer               string `json:"issuer"`
		WorkloadIdentityPool string `json:"workloadIdentityPool"`
		IdentityProvider     string `json:"identityProvider"`
	} `json:"authority"`

	// Membership labels - different from GKE labels
	Labels map[string]string
}
