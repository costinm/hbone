package handlers

import (
	"io/ioutil"
	"log"
	"os"
	"strings"

	"github.com/costinm/hbone"
	//otelprom "go.opentelemetry.io/otel/exporters/prometheus"

	"sigs.k8s.io/yaml"
)

func InitXDSCluster(hb *hbone.HBone) *hbone.Cluster {
	// Control plane - must be configured. May setup the cert using Citadel or meshca
	xdsC := hb.GetCluster("istiod.istio-system.svc:15012")
	if xdsC == nil {
		istiodCA := hb.GetEnv("CAROOT_ISTIOD", "")
		if istiodCA == "" {
			istiodCA = hb.GetEnv("CAROOT", "")
		}
		istiodAddr := hb.GetEnv("MCON_ADDR", "")
		if istiodAddr == "" {
			istiodAddr = hb.GetEnv("XDS_ADDR", "")
		}
		if istiodAddr != "" {
			// Istiod cluster, using tokens
			xdsC = hb.AddService(&hbone.Cluster{
				Addr:        istiodAddr + ":15012",
				TokenSource: "istio-ca",
				ID:          "istiod.istio-system.svc:15012",
				SNI:         "istiod.istio-system.svc",
				CACert:      istiodCA,
			})
		}
	}

	return xdsC
}

func LoadMeshConfig(hc *hbone.MeshSettings, path string) error {
	var cfg []byte
	var err error

	cfgEnv := os.Getenv("HBONE_CFG_YAML")
	if cfgEnv != "" {
		cfg = []byte(cfgEnv)
	} else {
		if path == "" {
			path = os.Getenv("HBONE_CFG")
		}
		if path == "" {
			path = "hbone.yaml"
		}
		if _, err := os.Stat(path); err == nil {
			cfg, err = ioutil.ReadFile(path)
			if err != nil {
				return err
			}
		}
	}

	err = yaml.Unmarshal(cfg, hc)
	if err != nil {
		log.Fatal("Failed to decode config", err)
	}

	// TODO: process env variables
	for _, kvs := range os.Environ() {
		kv := strings.SplitN(kvs, "=", 2)
		k := kv[0]
		switch k {
		case "NAMESPACE":
			hc.Namespace = kv[1]
		}

		if strings.HasPrefix(k, "PORT_") {
			hc.Ports[k[5:]] = kv[1]
		}
	}

	return err
}
