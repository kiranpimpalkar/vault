/*
Copyright © 2024 Ingka Holding B.V. All Rights Reserved.
Licensed under the GPL, Version 2 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

       <https://www.gnu.org/licenses/gpl-2.0.en.html>

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package exporter

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ingka-group/nutanix-exporter/internal/auth"
	"github.com/ingka-group/nutanix-exporter/internal/nutanix"
	"github.com/ingka-group/nutanix-exporter/internal/prom"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	ListenAddress  = ":9408"
	DefaultSection = "default"
)

var (
	ClusterPrefix string
	PCApiVersion  string
	VaultClient   *auth.VaultClient
	ClustersMap   map[string]*nutanix.Cluster
	clustersMu    sync.RWMutex // Protects ClustersMap
)

func Init() {

	// Get environment variables
	PCClusterName := getEnvOrFatal("PC_CLUSTER_NAME")
	PCClusterURL := getEnvOrFatal("PC_CLUSTER_URL")
	PCApiVersion := os.Getenv("PC_API_VERSION") // Optional, defaults to v4
	if PCApiVersion == "" {
		PCApiVersion = "v4"
	}
	ClusterPrefix = os.Getenv("CLUSTER_PREFIX") // Optional

	clusterRefreshIntervalStr := os.Getenv("CLUSTER_REFRESH_INTERVAL")
	clusterRefreshInterval := 0
	if clusterRefreshIntervalStr != "" {
		if v, err := strconv.Atoi(clusterRefreshIntervalStr); err == nil && v > 0 {
			clusterRefreshInterval = v
		}
	}

	vaultRefreshIntervalStr := os.Getenv("VAULT_REFRESH_INTERVAL")
	vaultRefreshInterval := 0
	if vaultRefreshIntervalStr != "" {
		if v, err := strconv.Atoi(vaultRefreshIntervalStr); err == nil && v > 0 {
			vaultRefreshInterval = v
		}
	}

	log.Printf("Initializing Vault client")
	vaultClient, err := auth.NewVaultClient()
	if err != nil {
		log.Fatalf("Failed to create Vault client: %v", err)
	}

	// Periodic refresh of vault client
	if vaultRefreshInterval > 0 {
		go func() {
			ticker := time.NewTicker(time.Duration(vaultRefreshInterval) * time.Second)
			defer ticker.Stop()

			for range ticker.C {
				log.Printf("Refreshing Vault client...")
				vaultClient, err = auth.NewVaultClient()
				if err != nil {
					log.Fatalf("Failed to refresh Vault client: %v", err)
				}
			}
		}()
	}

	log.Printf("Connecting to Prism Central")
	PCCluster := nutanix.NewCluster(PCClusterName, PCClusterURL, vaultClient, true, true, 10*time.Second)
	if PCCluster == nil {
		log.Fatalf("Failed to connect to Prism Central cluster")
	}

	// Initial setup of cluster list
	log.Printf("Initializing clusters")
	clusterMap, err := SetupClusters(PCCluster, vaultClient, PCApiVersion)
	if err != nil {
		log.Fatalf("Failed to initialize clusters: %v", err)
	}
	clustersMu.Lock()
	ClustersMap = clusterMap
	clustersMu.Unlock()

	// Periodic refresh of clusters
	if clusterRefreshInterval > 0 {
		go func() {
			ticker := time.NewTicker(time.Duration(clusterRefreshInterval) * time.Second)
			defer ticker.Stop()
			for range ticker.C { // Every time the ticker ticks, i.e. every refreshInterval secs, exec code below
				log.Printf("Refreshing cluster list...")
				newMap, err := SetupClusters(PCCluster, vaultClient, PCApiVersion)
				if err != nil {
					log.Printf("Cluster refresh failed: %v", err)
					continue // wait for next tick and try again
				}
				clustersMu.Lock()
				ClustersMap = newMap
				clustersMu.Unlock()
				log.Printf("Cluster list refreshed")
			}
		}()
	}

	log.Printf("Initializing HTTP server")
	http.HandleFunc("/", indexHandler)

	// Dynamically create metrics-serving handler for incoming http request
	http.HandleFunc("/metrics/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/metrics/")
		clustersMu.RLock()
		cluster, ok := ClustersMap[name]
		clustersMu.RUnlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		createClusterMetricsHandler(cluster, vaultClient)(w, r) // produce handler function for the incoming http request and execute it immediately
	})

	log.Printf("Starting Server on %s", ListenAddress)
	if err := http.ListenAndServe(ListenAddress, nil); err != nil {
		log.Fatalf("Error starting server: %s", err)
	}
}

// SetupClusters creates Prometheus collectors for every cluster registered in Prism Central
func SetupClusters(prismClient *nutanix.Cluster, vaultClient *auth.VaultClient, PCApiVersion string) (map[string]*nutanix.Cluster, error) {
	clusterData, err := FetchClusters(prismClient, PCApiVersion)
	if err != nil {
		return nil, err // Propagate the error up
	}

	clustersMap := make(map[string]*nutanix.Cluster)
	for name, url := range clusterData {
		cluster := nutanix.NewCluster(name, url, vaultClient, false, true, 10*time.Second)
		if cluster == nil {
			log.Printf("Failed to initialize cluster %s", name)
			continue
		}

		// Register collectors for this cluster
		log.Printf("Registering collectors for cluster %s", name)
		collectors := []prometheus.Collector{
			prom.NewStorageContainerCollector(cluster, "configs/storage_container.yaml"),
			prom.NewClusterCollector(cluster, "configs/cluster.yaml"),
			prom.NewHostCollector(cluster, "configs/host.yaml"),
			prom.NewVMCollector(cluster, "configs/vm.yaml"),
		}

		for _, collector := range collectors {
			cluster.Registry.MustRegister(collector)
		}
		cluster.Collectors = collectors

		// Add the cluster to the map
		clustersMap[name] = cluster
	}

	return clustersMap, nil
}

// FetchClusters fetches the name and IP of all Prism Element clusters registered in Prism Central.
// Takes a version flag to switch between v3 and v4 API calls. Skips clusters that don't match the prefix if provided.
func FetchClusters(prismClient *nutanix.Cluster, version string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	clusterData := make(map[string]string)

	// Define the functions for making requests and parsing for both v3 and v4.

	// v4b1 request function
	makeV4b1Request := func() (*http.Response, error) {
		return prismClient.API.MakeRequest(ctx, "GET", "/api/clustermgmt/v4.0.b1/config/clusters")
	}

	// v4 request function
	makeV4Request := func() (*http.Response, error) {
		return prismClient.API.MakeRequest(ctx, "GET", "/api/clustermgmt/v4.0/config/clusters")
	}

	// v3 request function
	makeV3Request := func() (*http.Response, error) {
		payload := map[string]interface{}{
			"kind":   "cluster",
			"length": 100, // Adjust as needed
			"offset": 0,
		}
		return prismClient.API.MakeRequestWithParams(ctx, "POST", "/api/nutanix/v3/clusters/list", nutanix.RequestParams{
			Payload: payload,
		})
	}

	// v4 parsing function
	parseV4Clusters := func(result map[string]interface{}) ([]map[string]string, error) {
		data, ok := result["data"].([]interface{})
		if !ok {
			return nil, fmt.Errorf("unexpected response format for v4")
		}

		var clusters []map[string]string
		for _, cluster := range data {
			clusterMap := cluster.(map[string]interface{})
			name, nameOk := clusterMap["name"].(string)
			if !nameOk || name == "Unnamed" {
				continue
			}
			network, networkOk := clusterMap["network"].(map[string]interface{})["externalAddress"].(map[string]interface{})
			if !networkOk || network["ipv4"] == nil {
				continue
			}
			ip, ipOk := network["ipv4"].(map[string]interface{})["value"].(string)
			if !ipOk {
				continue
			}

			clusters = append(clusters, map[string]string{
				"name": name,
				"ip":   ip,
			})
		}
		return clusters, nil
	}

	// v4b1 parsing function
	parseV4b1Clusters := parseV4Clusters

	// v3 parsing function
	parseV3Clusters := func(result map[string]interface{}) ([]map[string]string, error) {
		entities, ok := result["entities"].([]interface{})
		if !ok {
			return nil, fmt.Errorf("unexpected response format for v3")
		}

		var clusters []map[string]string
		for _, entity := range entities {
			cluster := entity.(map[string]interface{})
			spec, specOk := cluster["spec"].(map[string]interface{})
			status, statusOk := cluster["status"].(map[string]interface{})
			if !specOk || !statusOk {
				continue
			}

			name, nameOk := spec["name"].(string)
			if !nameOk || name == "Unnamed" {
				continue
			}

			network, networkOk := status["resources"].(map[string]interface{})["network"].(map[string]interface{})
			if !networkOk {
				continue
			}

			ip, ipOk := network["external_ip"].(string)
			if !ipOk {
				continue
			}

			clusters = append(clusters, map[string]string{
				"name": name,
				"ip":   ip,
			})
		}
		return clusters, nil
	}

	// Decide which request and parsing functions to use based on the version
	var resp *http.Response
	var err error
	var parseClusters func(map[string]interface{}) ([]map[string]string, error)

	if version == "v3" {
		resp, err = makeV3Request()
		parseClusters = parseV3Clusters
	} else if version == "v4b1" {
		resp, err = makeV4b1Request()
		parseClusters = parseV4b1Clusters
	} else {
		resp, err = makeV4Request()
		parseClusters = parseV4Clusters
	}

	if err != nil {
		return nil, err // Return the error to be handled by the caller
	}
	defer resp.Body.Close()

	// Parse the response
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	clusters, err := parseClusters(result)
	if err != nil {
		return nil, err
	}

	// Build the final clusterData map
	for _, cluster := range clusters {
		name := cluster["name"]
		ip := cluster["ip"]

		// Skip clusters that don't match the prefix if provided
		if ClusterPrefix != "" && !strings.HasPrefix(name, ClusterPrefix) {
			log.Printf("Skipping cluster %s", name)
			continue
		}

		clusterData[name] = fmt.Sprintf("https://%s:9440", ip)
		log.Printf("Found cluster %s at %s", name, clusterData[name])
	}

	return clusterData, nil
}

// createClusterMetricsHandler returns a http.HandlerFunc that serves metrics for a specific cluster
func createClusterMetricsHandler(cluster *nutanix.Cluster, vaultClient *auth.VaultClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Refresh credentials for the specific cluster
		cluster.RefreshCredentialsIfNeeded(vaultClient)

		// Serve metrics from the specific cluster's registry
		promhttp.HandlerFor(cluster.Registry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
	}
}

// indexHandler handles the / endpoint
func indexHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, `<html><head><title>Nutanix Exporter</title></head><body><h1>Nutanix Exporter</h1><p><a href="/metrics">Metrics</a></p></body></html>`)
}

// getEnvOrFatal returns the value of the specified environment variable or exits
func getEnvOrFatal(envVar string) string {
	value := os.Getenv(envVar)
	if value == "" {
		log.Fatalf("%s environment variable is not set", envVar)
	}
	return value
}
