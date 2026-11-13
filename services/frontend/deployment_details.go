// Modified by the mamori-sre-gke project: the deployment details are built in a local map and
// published under a lock, because requests read them while this goroutine was still writing (a
// data race the race detector reported). See CHANGES.md.

package main

import (
	"net/http"
	"os"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/sirupsen/logrus"
)

// deploymentDetailsMap is published once by loadDeploymentDetails and never changed afterwards;
// deploymentDetailsMu guards the handover. Read it with deploymentDetails().
var (
	deploymentDetailsMu  sync.RWMutex
	deploymentDetailsMap map[string]string
)
var log *logrus.Logger

// deploymentDetails returns the pod's hostname, cluster and zone, or nil while they are still
// loading. The map is not modified after it is published, so callers may read it freely.
func deploymentDetails() map[string]string {
	deploymentDetailsMu.RLock()
	defer deploymentDetailsMu.RUnlock()
	return deploymentDetailsMap
}

func init() {
	initializeLogger()
	// Use a goroutine to ensure loadDeploymentDetails()'s GCP API
	// calls don't block non-GCP deployments. See issue #685.
	go loadDeploymentDetails()
}

func initializeLogger() {
	log = logrus.New()
	log.Level = logrus.DebugLevel
	log.Formatter = &logrus.JSONFormatter{
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyTime:  "timestamp",
			logrus.FieldKeyLevel: "severity",
			logrus.FieldKeyMsg:   "message",
		},
		TimestampFormat: time.RFC3339Nano,
	}
	log.Out = os.Stdout
}

func loadDeploymentDetails() {
	details := make(map[string]string)
	var metaServerClient = metadata.NewClient(&http.Client{})

	podHostname, err := os.Hostname()
	if err != nil {
		log.Error("Failed to fetch the hostname for the Pod", err)
	}

	podCluster, err := metaServerClient.InstanceAttributeValue("cluster-name")
	if err != nil {
		log.Error("Failed to fetch the name of the cluster in which the pod is running", err)
	}

	podZone, err := metaServerClient.Zone()
	if err != nil {
		log.Error("Failed to fetch the Zone of the node where the pod is scheduled", err)
	}

	details["HOSTNAME"] = podHostname
	details["CLUSTERNAME"] = podCluster
	details["ZONE"] = podZone
	deploymentDetailsMu.Lock()
	deploymentDetailsMap = details
	deploymentDetailsMu.Unlock()

	log.WithFields(logrus.Fields{
		"cluster":  podCluster,
		"zone":     podZone,
		"hostname": podHostname,
	}).Debug("Loaded deployment details")
}
