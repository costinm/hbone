// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8s

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"net"
	"net/url"
	"strings"

	"github.com/costinm/hbone"
)

func kubeconfig2Rest(uk *hbone.HBone, name string, cluster *KubeCluster, user *KubeUser, ns string) (*hbone.Cluster, error) {
	if ns == "" {
		ns = "default"
	}
	u, err := url.Parse(cluster.Server)
	h := u.Hostname()
	p := u.Port()
	if err != nil {
		return nil, err
	}
	if p == "" {
		p = "443"
	}
	rc := &hbone.Cluster{
		Addr: net.JoinHostPort(h, p),
		Path: u.Path,
	}
	if user.Token != "" {
		// TODO: reload kube config to detect token change
		// This is the long-lived JWT token
		rc.TokenProvider = func(ctx context.Context, s string) (string, error) {
			return user.Token, nil
		}
	}

	parts := strings.Split(name, "_")
	if parts[0] == "gke" {
		//rc.ProjectId = parts[1]
		rc.Location = parts[2]
	}
	rc.ID = name

	// May be useful to AddService: strings.HasPrefix(name, "gke_") ||
	if user.AuthProvider.Name != "" {
		rc.TokenProvider = uk.AuthProviders[user.AuthProvider.Name]
		if rc.TokenProvider == nil {
			return nil, errors.New("Missing provider " + user.AuthProvider.Name)
		}
	}

	// TODO: support client cert, token file (with reload)
	caCert, err := base64.StdEncoding.DecodeString(string(cluster.CertificateAuthorityData))
	if err != nil {
		return nil, err
	}

	//caCert := cluster.CertificateAuthorityData
	rc.CACert = string(caCert)

	rc.Client = uk.HttpClient(caCert)

	return rc, nil
}

func GKEClusterName(id string) (projectID, location, name string) {
	parts := strings.Split(name, "_")
	if parts[0] == "gke" && len(parts) >= 4 {
		return parts[1], parts[2], parts[3]
	}
	return "", "", id
}

// AddKubeConfigClusters extracts supported RestClusters from the kube config, returns the default and the list
// of clusters by location.
// GKE naming conventions are assumed for extracting the location.
//
// URest is used to configure TokenProvider and as factory for the http client.
// Returns the default client and the list of non-default clients.
func AddKubeConfigClusters(uk *hbone.HBone, kc *KubeConfig) (*hbone.Cluster, map[string]*hbone.Cluster, error) {
	var cluster *KubeCluster
	var user *KubeUser

	cByName := map[string]*hbone.Cluster{}

	if len(kc.Contexts) == 0 || kc.CurrentContext == "" {
		if len(kc.Clusters) == 0 || len(kc.Users) == 0 {
			return nil, cByName, errors.New("Kubeconfig has no clusters")
		}
		user = &kc.Users[0].User
		cluster = &kc.Clusters[0].Cluster
		rc, err := kubeconfig2Rest(uk, "default", cluster, user, "default")
		uk.AddService(rc)

		if err != nil {
			return nil, nil, err
		}
		return rc, nil, nil
	}

	// Have contexts
	for _, cc := range kc.Contexts {
		for _, c := range kc.Clusters {
			c := c
			if c.Name == cc.Context.Cluster {
				cluster = &c.Cluster
			}
		}
		for _, c := range kc.Users {
			c := c
			if c.Name == cc.Context.User {
				user = &c.User
			}
		}
		cc := cc
		rc, err := kubeconfig2Rest(uk, cc.Context.Cluster, cluster, user, cc.Context.Namespace)
		if err != nil {
			log.Println("Skipping incompatible cluster ", cc.Context.Cluster, err)
		} else {
			uk.AddService(rc)
			//cByLoc[rc.Location] = append(cByLoc[rc.Location], rc)
			cByName[cc.Name] = rc
		}
	}

	if len(cByName) == 0 {
		return nil, nil, errors.New("no clusters found")
	}
	defc := cByName[kc.CurrentContext]
	if defc == nil {
		for _, c := range cByName {
			defc = c
			break
		}
	}
	uk.AddService(defc)
	return defc, cByName, nil
}
