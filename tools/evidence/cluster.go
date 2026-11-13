package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// gkeCluster is the part of the GKE API's Cluster resource (projects.locations.clusters.get) that
// the bundle uses. The workflow drops everything else, such as the endpoint and CA certificate.
type gkeCluster struct {
	Name                 string `json:"name"`
	Location             string `json:"location"`
	Status               string `json:"status"`
	CurrentMasterVersion string `json:"currentMasterVersion"`
	CurrentNodeVersion   string `json:"currentNodeVersion"`
	ReleaseChannel       struct {
		Channel string `json:"channel"`
	} `json:"releaseChannel"`
	NodePools []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Status  string `json:"status"`
		Config  struct {
			MachineType string `json:"machineType"`
			Spot        bool   `json:"spot"`
		} `json:"config"`
		Autoscaling struct {
			Enabled bool `json:"enabled"`
		} `json:"autoscaling"`
	} `json:"nodePools"`
}

// nodeList is `kubectl get nodes -o json`.
type nodeList struct {
	Items []struct {
		Metadata struct {
			Name              string            `json:"name"`
			CreationTimestamp string            `json:"creationTimestamp"`
			Labels            map[string]string `json:"labels"`
		} `json:"metadata"`
		Status struct {
			NodeInfo struct {
				KubeletVersion          string `json:"kubeletVersion"`
				OSImage                 string `json:"osImage"`
				ContainerRuntimeVersion string `json:"containerRuntimeVersion"`
			} `json:"nodeInfo"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

type clusterData struct {
	Name                string     `json:"name"`
	Location            string     `json:"location"`
	Status              string     `json:"status"`
	ReleaseChannel      string     `json:"releaseChannel"`
	ControlPlaneVersion string     `json:"controlPlaneVersion"`
	NodePools           []nodePool `json:"nodePools"`
	// The node list comes from the Kubernetes API, a second input; it can be missing on its own.
	NodesStatus string `json:"nodesStatus"`
	NodesReason string `json:"nodesReason,omitempty"`
	Nodes       []node `json:"nodes"`
}

type nodePool struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Status      string `json:"status"`
	MachineType string `json:"machineType"`
	Spot        bool   `json:"spot"`
	Autoscaling bool   `json:"autoscaling"`
}

type node struct {
	Name             string `json:"name"`
	Pool             string `json:"pool"`
	KubeletVersion   string `json:"kubeletVersion"`
	Ready            bool   `json:"ready"`
	Spot             bool   `json:"spot"`
	CreatedAt        string `json:"createdAt"`
	OSImage          string `json:"osImage"`
	ContainerRuntime string `json:"containerRuntime"`
}

func clusterSection(clusterIn, nodesIn input) section[clusterData] {
	s := newSection("Cluster versions and nodes", clusterIn, parseGKECluster)
	s.Inputs = append(s.Inputs, nodesIn.ref)
	if s.Status != statusCollected {
		return s
	}

	d := s.Data
	d.Nodes = []node{}
	switch {
	case !nodesIn.ok():
		d.NodesStatus, d.NodesReason = statusNotCollected, nodesIn.ref.Reason
	default:
		nodes, err := parseNodes(nodesIn.data)
		if err != nil {
			d.NodesStatus, d.NodesReason = statusNotCollected, fmt.Sprintf("%s is not valid: %v", nodesIn.ref.File, err)
			break
		}
		d.NodesStatus, d.Nodes = statusCollected, nodes
	}

	s.Attention, s.Headline = clusterVerdict(d)
	return s
}

func parseGKECluster(data []byte) (*clusterData, bool, string, error) {
	var c gkeCluster
	if err := decode(data, &c); err != nil {
		return nil, false, "", err
	}
	if c.Name == "" || c.CurrentMasterVersion == "" {
		return nil, false, "", fmt.Errorf("name or currentMasterVersion is missing")
	}
	d := &clusterData{
		Name:                c.Name,
		Location:            c.Location,
		Status:              c.Status,
		ReleaseChannel:      c.ReleaseChannel.Channel,
		ControlPlaneVersion: c.CurrentMasterVersion,
		NodePools:           []nodePool{},
	}
	for _, p := range c.NodePools {
		d.NodePools = append(d.NodePools, nodePool{
			Name: p.Name, Version: p.Version, Status: p.Status,
			MachineType: p.Config.MachineType, Spot: p.Config.Spot, Autoscaling: p.Autoscaling.Enabled,
		})
	}
	slices.SortFunc(d.NodePools, func(a, b nodePool) int { return cmp.Compare(a.Name, b.Name) })
	// clusterSection sets the verdict once the nodes are known.
	return d, false, "", nil
}

func parseNodes(data []byte) ([]node, error) {
	var l nodeList
	if err := decode(data, &l); err != nil {
		return nil, err
	}
	nodes := []node{}
	for _, it := range l.Items {
		n := node{
			Name:             it.Metadata.Name,
			Pool:             it.Metadata.Labels["cloud.google.com/gke-nodepool"],
			KubeletVersion:   it.Status.NodeInfo.KubeletVersion,
			Spot:             it.Metadata.Labels["cloud.google.com/gke-spot"] == "true",
			CreatedAt:        it.Metadata.CreationTimestamp,
			OSImage:          it.Status.NodeInfo.OSImage,
			ContainerRuntime: it.Status.NodeInfo.ContainerRuntimeVersion,
		}
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" {
				n.Ready = c.Status == "True"
			}
		}
		nodes = append(nodes, n)
	}
	slices.SortFunc(nodes, func(a, b node) int { return cmp.Compare(a.Name, b.Name) })
	return nodes, nil
}

// clusterVerdict flags a cluster that isn't RUNNING, a node pool that isn't RUNNING, and nodes that
// aren't Ready or are missing. Version skew between control plane and nodes is normal during an
// upgrade, so it is shown but not flagged.
func clusterVerdict(d *clusterData) (bool, string) {
	var problems []string
	if d.Status != "RUNNING" {
		problems = append(problems, "cluster status "+cmp.Or(d.Status, "unknown"))
	}
	for _, p := range d.NodePools {
		if p.Status != "RUNNING" {
			problems = append(problems, fmt.Sprintf("node pool %s %s", p.Name, cmp.Or(p.Status, "status unknown")))
		}
	}

	poolVersions := []string{}
	for _, p := range d.NodePools {
		if !slices.Contains(poolVersions, p.Version) {
			poolVersions = append(poolVersions, p.Version)
		}
	}
	parts := []string{
		cmp.Or(d.Status, "status unknown"),
		"control plane " + d.ControlPlaneVersion,
		"node pools " + cmp.Or(strings.Join(poolVersions, ", "), "none"),
	}

	if d.NodesStatus == statusCollected {
		ready := 0
		for _, n := range d.Nodes {
			if n.Ready {
				ready++
			}
		}
		parts = append(parts, fmt.Sprintf("%d/%d nodes Ready", ready, len(d.Nodes)))
		if len(d.Nodes) == 0 {
			problems = append(problems, "no nodes")
		} else if ready < len(d.Nodes) {
			problems = append(problems, plural(len(d.Nodes)-ready, "node not Ready", "nodes not Ready"))
		}
	} else {
		parts = append(parts, "nodes not collected: "+d.NodesReason)
	}
	if d.ReleaseChannel != "" {
		parts = append(parts, "channel "+d.ReleaseChannel)
	}

	headline := strings.Join(parts, ", ")
	if len(problems) > 0 {
		headline += " (" + strings.Join(problems, "; ") + ")"
	}
	return len(problems) > 0, headline
}
