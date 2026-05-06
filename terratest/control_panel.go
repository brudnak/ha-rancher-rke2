package test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brudnak/ha-rancher-rke2/terratest/ui"
	"github.com/spf13/viper"
)

type localControlPanel struct {
	token    string
	totalHAs int
	repoRoot string
	testDir  string
	listener net.Listener
	server   *http.Server
	baseURL  string
	doneCh   chan error

	mu                sync.Mutex
	cleanupRunning    bool
	cleanupOutput     []string
	cleanupStartedAt  *time.Time
	cleanupFinishedAt *time.Time
	cleanupError      string

	rancherTokens             map[int]string
	downstreamKubeconfigCache map[string]string
}

type panelState struct {
	Clusters panelClusterState `json:"clusters"`
	Cleanup  cleanupState      `json:"cleanup"`
}

type panelClusterState struct {
	Items []clusterView `json:"items"`
}

type clusterView struct {
	ID                  string    `json:"id"`
	Type                string    `json:"type"`
	HAIndex             int       `json:"haIndex"`
	Name                string    `json:"name"`
	Version             string    `json:"version,omitempty"`
	RancherURL          string    `json:"rancherUrl,omitempty"`
	LoadBalancer        string    `json:"loadBalancer,omitempty"`
	Namespace           string    `json:"namespace,omitempty"`
	ManagementClusterID string    `json:"managementClusterId,omitempty"`
	KubeconfigPath      string    `json:"kubeconfigPath,omitempty"`
	DownloadName        string    `json:"downloadName,omitempty"`
	Provisioning        bool      `json:"provisioning,omitempty"`
	ProvisioningMessage string    `json:"provisioningMessage,omitempty"`
	Available           bool      `json:"available"`
	Reachable           bool      `json:"reachable"`
	Error               string    `json:"error,omitempty"`
	Pods                []podView `json:"pods"`
}

type podView struct {
	Namespace   string `json:"namespace,omitempty"`
	Name        string `json:"name"`
	Ready       string `json:"ready"`
	Status      string `json:"status"`
	Restarts    int    `json:"restarts"`
	Age         string `json:"age"`
	Node        string `json:"node,omitempty"`
	Containers  string `json:"containers"`
	Leader      bool   `json:"leader"`
	LeaderLabel string `json:"leaderLabel,omitempty"`
}

type cleanupState struct {
	Running    bool       `json:"running"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Error      string     `json:"error,omitempty"`
	Output     []string   `json:"output"`
}

type kubectlPodList struct {
	Items []kubectlPod `json:"items"`
}

type kubectlPod struct {
	Metadata struct {
		Namespace         string    `json:"namespace"`
		Name              string    `json:"name"`
		CreationTimestamp time.Time `json:"creationTimestamp"`
	} `json:"metadata"`
	Spec struct {
		NodeName   string `json:"nodeName"`
		Containers []struct {
			Name string `json:"name"`
		} `json:"containers"`
		InitContainers []struct {
			Name string `json:"name"`
		} `json:"initContainers"`
	} `json:"spec"`
	Status struct {
		Phase             string `json:"phase"`
		Reason            string `json:"reason"`
		ContainerStatuses []struct {
			Name         string `json:"name"`
			Ready        bool   `json:"ready"`
			RestartCount int    `json:"restartCount"`
			State        struct {
				Waiting struct {
					Reason string `json:"reason"`
				} `json:"waiting"`
				Terminated struct {
					Reason string `json:"reason"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
		InitContainerStatuses []struct {
			Name         string `json:"name"`
			Ready        bool   `json:"ready"`
			RestartCount int    `json:"restartCount"`
			State        struct {
				Waiting struct {
					Reason string `json:"reason"`
				} `json:"waiting"`
				Terminated struct {
					Reason string `json:"reason"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"initContainerStatuses"`
	} `json:"status"`
}

type provisioningClusterList struct {
	Items []provisioningClusterItem `json:"items"`
}

type provisioningClusterItem struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Status struct {
		ClusterName string `json:"clusterName"`
	} `json:"status"`
}

type managementClusterList struct {
	Items []managementClusterItem `json:"items"`
}

type managementClusterItem struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		DisplayName string `json:"displayName"`
	} `json:"spec"`
}

type discoveredDownstreamCluster struct {
	Name                string
	Namespace           string
	ManagementClusterID string
}

func newLocalControlPanel(totalHAs int) (*localControlPanel, error) {
	token, err := randomConfirmationToken()
	if err != nil {
		return nil, fmt.Errorf("failed to create control panel token: %w", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to determine working directory: %w", err)
	}
	repoRoot, testDir, err := resolveControlPanelPaths(cwd)
	if err != nil {
		return nil, err
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("failed to start control panel listener: %w", err)
	}

	panel := &localControlPanel{
		token:                     token,
		totalHAs:                  totalHAs,
		repoRoot:                  repoRoot,
		testDir:                   testDir,
		listener:                  listener,
		baseURL:                   fmt.Sprintf("http://%s/?token=%s", listener.Addr().String(), token),
		doneCh:                    make(chan error, 1),
		rancherTokens:             map[int]string{},
		downstreamKubeconfigCache: map[string]string{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", panel.handleIndex)
	mux.HandleFunc("/static/control_panel.js", panel.handleControlPanelJS)
	mux.HandleFunc("/static/control_panel_theme.js", panel.handleControlPanelThemeJS)
	mux.HandleFunc("/api/state", panel.handleState)
	mux.HandleFunc("/api/logs", panel.handleLogs)
	mux.HandleFunc("/api/logs/stream", panel.handleLogStream)
	mux.HandleFunc("/api/kubeconfig", panel.handleKubeconfigDownload)
	mux.HandleFunc("/api/cleanup", panel.handleCleanup)
	mux.HandleFunc("/api/shutdown", panel.handleShutdown)

	panel.server = &http.Server{Handler: mux}
	return panel, nil
}

func (p *localControlPanel) start() {
	go func() {
		err := p.server.Serve(p.listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.doneCh <- err
			return
		}
		p.doneCh <- nil
	}()
}

func (p *localControlPanel) wait() error {
	return <-p.doneCh
}

func (p *localControlPanel) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !p.authorized(r) {
		http.Error(w, "invalid control panel token", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	page := template.Must(template.New("control-panel").Parse(ui.ControlPanelHTML))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = page.Execute(w, struct {
		Token string
	}{
		Token: p.token,
	})
}

func (p *localControlPanel) handleControlPanelJS(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedLocalBrowserRead(r) {
		http.Error(w, "invalid control panel token", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(ui.ControlPanelJS))
}

func (p *localControlPanel) handleControlPanelThemeJS(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedLocalBrowserRead(r) {
		http.Error(w, "invalid control panel token", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(ui.ControlPanelThemeJS))
}

func (p *localControlPanel) handleState(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedReadOnly(r) {
		http.Error(w, "invalid control panel token", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	state := p.buildState()
	writeJSON(w, state)
}

func (p *localControlPanel) handleLogs(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedReadOnly(r) {
		http.Error(w, "invalid control panel token", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cluster, pod, namespace, container, err := p.logRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	args := []string{"logs", pod, "-n", namespace, "--tail=200"}
	if container != "" {
		args = append(args, "-c", container)
	} else {
		args = append(args, "--all-containers=true")
	}

	output, err := runKubectl(cluster.KubeconfigPath, args...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	writeJSON(w, map[string]string{"text": output})
}

func (p *localControlPanel) handleLogStream(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedReadOnly(r) {
		http.Error(w, "invalid control panel token", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	cluster, pod, namespace, container, err := p.logRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	args := []string{"logs", "-f", pod, "-n", namespace, "--tail=20"}
	if container != "" {
		args = append(args, "-c", container)
	} else {
		args = append(args, "--all-containers=true")
	}

	cmd := exec.CommandContext(r.Context(), "kubectl", append([]string{"--kubeconfig", cluster.KubeconfigPath}, args...)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to open log stream: %v", err), http.StatusInternalServerError)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to open log stream: %v", err), http.StatusInternalServerError)
		return
	}
	if err := cmd.Start(); err != nil {
		http.Error(w, fmt.Sprintf("failed to start log stream: %v", err), http.StatusBadGateway)
		return
	}
	defer cmd.Wait()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sendLine := func(eventName, line string) {
		fmt.Fprintf(w, "event: %s\n", eventName)
		fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(line, "\n", "\\n"))
		flusher.Flush()
	}

	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			sendLine("line", scanner.Text())
		}
	}()

	stderrBytes, _ := io.ReadAll(stderr)
	<-stdoutDone
	if len(stderrBytes) > 0 {
		sendLine("error", string(stderrBytes))
	}
	sendLine("end", "stream closed")
}

func (p *localControlPanel) handleKubeconfigDownload(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedLocalBrowserRead(r) {
		http.Error(w, "invalid control panel token", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clusterID := strings.TrimSpace(r.URL.Query().Get("cluster"))
	if clusterID == "" {
		http.Error(w, "cluster is required", http.StatusBadRequest)
		return
	}

	cluster, err := p.clusterByID(clusterID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	content, filename, err := p.kubeconfigContentForDownload(cluster)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/x-yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	_, _ = w.Write(content)
}

func (p *localControlPanel) handleCleanup(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedLocalAction(r) {
		http.Error(w, "invalid control panel token", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(strings.ToLower(req.Confirm)) != "cleanup" {
		http.Error(w, "typed confirmation must equal cleanup", http.StatusBadRequest)
		return
	}

	if err := p.startCleanup(); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	writeJSON(w, map[string]string{"status": "cleanup started"})
}

func (p *localControlPanel) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if !p.authorizedLocalAction(r) {
		http.Error(w, "invalid control panel token", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, map[string]string{"status": "shutting down"})

	go func() {
		time.Sleep(150 * time.Millisecond)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.server.Shutdown(shutdownCtx)
	}()
}

func (p *localControlPanel) buildState() panelState {
	return panelState{
		Clusters: panelClusterState{
			Items: p.discoverClusters(),
		},
		Cleanup: p.snapshotCleanupState(),
	}
}

func (p *localControlPanel) snapshotCleanupState() cleanupState {
	p.mu.Lock()
	defer p.mu.Unlock()

	outputCopy := append([]string(nil), p.cleanupOutput...)
	if outputCopy == nil {
		outputCopy = []string{}
	}
	return cleanupState{
		Running:    p.cleanupRunning,
		StartedAt:  p.cleanupStartedAt,
		FinishedAt: p.cleanupFinishedAt,
		Error:      p.cleanupError,
		Output:     outputCopy,
	}
}

func (p *localControlPanel) discoverClusters() []clusterView {
	outputs, _ := readTerraformFlatOutputs(p.repoRoot)
	versions := readRequestedRancherVersionsForPanel(p.totalHAs)
	downstreamRecords, _ := readDownstreamOutputRecords()
	recordsByHA := downstreamRecordsByHA(downstreamRecords)

	clusters := make([]clusterView, 0, p.totalHAs)
	for i := 1; i <= p.totalHAs; i++ {
		cluster := clusterView{
			ID:           localClusterID(i),
			Type:         "local",
			HAIndex:      i,
			Name:         fmt.Sprintf("HA %d Local", i),
			DownloadName: fmt.Sprintf("local-ha-%d.yaml", i),
		}
		if len(versions) >= i {
			cluster.Version = versions[i-1]
		}
		cluster.KubeconfigPath = filepath.Join(p.testDir, fmt.Sprintf("high-availability-%d", i), "kube_config.yaml")
		if outputs != nil {
			cluster.RancherURL = clickableURL(outputs[fmt.Sprintf("ha_%d_rancher_url", i)])
			cluster.LoadBalancer = outputs[fmt.Sprintf("ha_%d_aws_lb", i)]
		}

		if _, err := os.Stat(cluster.KubeconfigPath); err != nil {
			cluster.Error = "kubeconfig not found"
			clusters = append(clusters, cluster)
			continue
		}

		cluster.Available = true
		pods, err := fetchLocalRancherPods(cluster.KubeconfigPath)
		if err != nil {
			cluster.Error = err.Error()
			clusters = append(clusters, cluster)
			clusters = append(clusters, p.discoverDownstreamClusters(cluster, recordsByHA[i])...)
			continue
		}

		cluster.Reachable = true
		cluster.Pods = pods
		clusters = append(clusters, cluster)
		clusters = append(clusters, p.discoverDownstreamClusters(cluster, recordsByHA[i])...)
	}

	return clusters
}

func (p *localControlPanel) discoverDownstreamClusters(local clusterView, records []downstreamOutputRecord) []clusterView {
	if !local.Available {
		return nil
	}

	provisioningClusters, err := discoverProvisioningDownstreamClusters(local.KubeconfigPath)
	if err != nil {
		return downstreamClustersFromRecords(local, records, err)
	}

	recordByName := downstreamRecordsByClusterKey(records)
	activeIDs := map[string]bool{}
	clusters := make([]clusterView, 0, len(provisioningClusters))
	for _, item := range provisioningClusters {
		key := provisioningClusterRecordKey(item.Namespace, item.Name)
		record := recordByName[key]
		clusterID := downstreamClusterID(local.HAIndex, item.Namespace, item.Name)
		activeIDs[clusterID] = true
		cluster := clusterView{
			ID:                  clusterID,
			Type:                "downstream",
			HAIndex:             local.HAIndex,
			Name:                item.Name,
			Version:             record.K3SVersion,
			RancherURL:          local.RancherURL,
			Namespace:           item.Namespace,
			ManagementClusterID: item.ManagementClusterID,
			DownloadName:        safeKubeconfigDownloadName(item.Name),
			Available:           true,
		}
		if record.KubeconfigPath != "" {
			cluster.KubeconfigPath = record.KubeconfigPath
		}
		if cluster.ManagementClusterID == "" {
			cluster.Provisioning = true
			cluster.ProvisioningMessage = "Waiting for Rancher to assign a downstream cluster id"
			clusters = append(clusters, cluster)
			continue
		}

		kubeconfigPath, err := p.ensureDownstreamKubeconfig(local.HAIndex, local.RancherURL, cluster.ID, item.ManagementClusterID, record.KubeconfigPath)
		if err != nil {
			cluster.Provisioning = true
			cluster.ProvisioningMessage = "Waiting for downstream kubeconfig"
			clusters = append(clusters, cluster)
			continue
		}
		cluster.KubeconfigPath = kubeconfigPath

		pods, err := fetchAllPods(kubeconfigPath)
		if err != nil {
			cluster.Provisioning = true
			cluster.ProvisioningMessage = "Waiting for downstream Kubernetes API"
			clusters = append(clusters, cluster)
			continue
		}
		cluster.Reachable = true
		cluster.Pods = pods
		clusters = append(clusters, cluster)
	}
	p.pruneStaleDownstreamKubeconfigs(local.HAIndex, activeIDs)

	return clusters
}

func downstreamClustersFromRecords(local clusterView, records []downstreamOutputRecord, discoverErr error) []clusterView {
	clusters := make([]clusterView, 0, len(records))
	for _, record := range records {
		cluster := clusterView{
			ID:                  downstreamClusterID(local.HAIndex, record.Namespace, record.ClusterName),
			Type:                "downstream",
			HAIndex:             local.HAIndex,
			Name:                record.ClusterName,
			Version:             record.K3SVersion,
			RancherURL:          local.RancherURL,
			Namespace:           record.Namespace,
			ManagementClusterID: record.ManagementClusterID,
			KubeconfigPath:      record.KubeconfigPath,
			DownloadName:        safeKubeconfigDownloadName(record.ClusterName),
			Available:           record.KubeconfigPath != "" || record.ManagementClusterID != "",
			Provisioning:        true,
			ProvisioningMessage: fmt.Sprintf("Waiting for downstream discovery (%v)", discoverErr),
		}
		clusters = append(clusters, cluster)
	}
	return clusters
}

func discoverProvisioningDownstreamClusters(kubeconfigPath string) ([]discoveredDownstreamCluster, error) {
	output, err := runKubectl(kubeconfigPath, "get", "clusters.provisioning.cattle.io", "-A", "-o", "json")
	if err != nil {
		return nil, err
	}

	var list provisioningClusterList
	if err := json.Unmarshal([]byte(output), &list); err != nil {
		return nil, fmt.Errorf("failed to parse provisioning clusters: %w", err)
	}

	clusters := make([]discoveredDownstreamCluster, 0, len(list.Items))
	for _, item := range list.Items {
		name := strings.TrimSpace(item.Metadata.Name)
		namespace := strings.TrimSpace(item.Metadata.Namespace)
		if name == "" || namespace == "" {
			continue
		}
		if name == "local" || namespace == "local" {
			continue
		}
		clusters = append(clusters, discoveredDownstreamCluster{
			Name:                name,
			Namespace:           namespace,
			ManagementClusterID: strings.TrimSpace(item.Status.ClusterName),
		})
	}

	managementClusters, err := discoverManagementDownstreamClusters(kubeconfigPath)
	if err == nil {
		seenManagementIDs := map[string]bool{}
		for _, cluster := range clusters {
			if cluster.ManagementClusterID != "" {
				seenManagementIDs[cluster.ManagementClusterID] = true
			}
		}
		for _, cluster := range managementClusters {
			if seenManagementIDs[cluster.ManagementClusterID] {
				continue
			}
			clusters = append(clusters, cluster)
		}
	}

	sort.Slice(clusters, func(i, j int) bool {
		left := provisioningClusterRecordKey(clusters[i].Namespace, clusters[i].Name)
		right := provisioningClusterRecordKey(clusters[j].Namespace, clusters[j].Name)
		return left < right
	})
	return clusters, nil
}

func discoverManagementDownstreamClusters(kubeconfigPath string) ([]discoveredDownstreamCluster, error) {
	output, err := runKubectl(kubeconfigPath, "get", "clusters.management.cattle.io", "-o", "json")
	if err != nil {
		return nil, err
	}

	var list managementClusterList
	if err := json.Unmarshal([]byte(output), &list); err != nil {
		return nil, fmt.Errorf("failed to parse management clusters: %w", err)
	}

	clusters := make([]discoveredDownstreamCluster, 0, len(list.Items))
	for _, item := range list.Items {
		clusterID := strings.TrimSpace(item.Metadata.Name)
		if clusterID == "" || clusterID == "local" {
			continue
		}
		name := strings.TrimSpace(item.Spec.DisplayName)
		if name == "" {
			name = clusterID
		}
		clusters = append(clusters, discoveredDownstreamCluster{
			Name:                name,
			ManagementClusterID: clusterID,
		})
	}
	return clusters, nil
}

func downstreamRecordsByHA(records []downstreamOutputRecord) map[int][]downstreamOutputRecord {
	byHA := map[int][]downstreamOutputRecord{}
	for _, record := range records {
		byHA[record.HAIndex] = append(byHA[record.HAIndex], record)
	}
	return byHA
}

func downstreamRecordsByClusterKey(records []downstreamOutputRecord) map[string]downstreamOutputRecord {
	byName := map[string]downstreamOutputRecord{}
	for _, record := range records {
		key := provisioningClusterRecordKey(record.Namespace, record.ClusterName)
		byName[key] = record
	}
	return byName
}

func provisioningClusterRecordKey(namespace, name string) string {
	return strings.TrimSpace(namespace) + "/" + strings.TrimSpace(name)
}

func localClusterID(instanceNum int) string {
	return fmt.Sprintf("ha-%d-local", instanceNum)
}

func downstreamClusterID(instanceNum int, namespace, name string) string {
	namespacePart := sanitizeIDPart(namespace)
	namePart := sanitizeIDPart(name)
	if namespacePart == "" {
		return fmt.Sprintf("ha-%d-downstream-%s", instanceNum, namePart)
	}
	return fmt.Sprintf("ha-%d-downstream-%s-%s", instanceNum, namespacePart, namePart)
}

func sanitizeIDPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func safeKubeconfigDownloadName(clusterName string) string {
	name := sanitizeIDPart(clusterName)
	if name == "" {
		name = "downstream"
	}
	return name + ".yaml"
}

func (p *localControlPanel) pruneStaleDownstreamKubeconfigs(haIndex int, activeIDs map[string]bool) {
	prefix := fmt.Sprintf("ha-%d-downstream-", haIndex)

	p.mu.Lock()
	for clusterID, path := range p.downstreamKubeconfigCache {
		if !strings.HasPrefix(clusterID, prefix) || activeIDs[clusterID] {
			continue
		}
		delete(p.downstreamKubeconfigCache, clusterID)
		RemoveFile(path)
	}
	p.mu.Unlock()

	cacheDir := filepath.Join(automationOutputDir(), "control-panel")
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || filepath.Ext(name) != ".yaml" {
			continue
		}
		clusterID := strings.TrimSuffix(name, ".yaml")
		if activeIDs[clusterID] {
			continue
		}
		RemoveFile(filepath.Join(cacheDir, name))
	}
}

func fetchLocalRancherPods(kubeconfigPath string) ([]podView, error) {
	pods, err := fetchPods(kubeconfigPath, "cattle-system")
	if err != nil {
		return nil, err
	}

	filtered := make([]podView, 0, len(pods))
	for _, pod := range pods {
		nameLower := strings.ToLower(pod.Name)
		if !strings.Contains(nameLower, "rancher") && !strings.Contains(nameLower, "webhook") {
			continue
		}
		filtered = append(filtered, pod)
	}
	return filtered, nil
}

func fetchAllPods(kubeconfigPath string) ([]podView, error) {
	return fetchPods(kubeconfigPath, "")
}

func fetchRelevantPods(kubeconfigPath string) ([]podView, error) {
	return fetchLocalRancherPods(kubeconfigPath)
}

func fetchPods(kubeconfigPath, namespace string) ([]podView, error) {
	args := []string{"get", "pods"}
	if namespace == "" {
		args = append(args, "-A")
	} else {
		args = append(args, "-n", namespace)
	}
	args = append(args, "-o", "json")

	output, err := runKubectl(kubeconfigPath, args...)
	if err != nil {
		return nil, err
	}

	var list kubectlPodList
	if err := json.Unmarshal([]byte(output), &list); err != nil {
		return nil, fmt.Errorf("failed to parse pod list: %w", err)
	}

	leaderLabels := discoverLeaderLabels(kubeconfigPath)

	pods := make([]podView, 0)
	for _, item := range list.Items {
		totalContainers := len(item.Spec.Containers)
		readyContainers := 0
		restarts := 0
		status := item.Status.Phase
		for _, containerStatus := range item.Status.ContainerStatuses {
			if containerStatus.Ready {
				readyContainers++
			}
			restarts += containerStatus.RestartCount
			if containerStatus.State.Waiting.Reason != "" {
				status = containerStatus.State.Waiting.Reason
			}
			if containerStatus.State.Terminated.Reason != "" {
				status = containerStatus.State.Terminated.Reason
			}
		}
		if item.Status.Reason != "" {
			status = item.Status.Reason
		}

		containerNames := make([]string, 0, len(item.Spec.Containers))
		for _, container := range item.Spec.Containers {
			containerNames = append(containerNames, container.Name)
		}

		leaderLabel := leaderLabels[item.Metadata.Name]
		pods = append(pods, podView{
			Namespace:   item.Metadata.Namespace,
			Name:        item.Metadata.Name,
			Ready:       fmt.Sprintf("%d/%d", readyContainers, totalContainers),
			Status:      status,
			Restarts:    restarts,
			Age:         humanDurationSince(item.Metadata.CreationTimestamp),
			Node:        item.Spec.NodeName,
			Containers:  strings.Join(containerNames, ", "),
			Leader:      leaderLabel != "",
			LeaderLabel: leaderLabel,
		})
	}

	sort.Slice(pods, func(i, j int) bool {
		return pods[i].Name < pods[j].Name
	})

	return pods, nil
}

func discoverLeaderLabels(kubeconfigPath string) map[string]string {
	leaders := map[string]string{}
	if holder, err := leaseHolderIdentity(kubeconfigPath, "kube-system", "cattle-controllers"); err == nil && holder != "" {
		leaders[holder] = "Leader"
	}
	if holder, err := leaseHolderIdentity(kubeconfigPath, "cattle-system", "rancher-webhook-leader"); err == nil && holder != "" {
		leaders[holder] = "Webhook Leader"
	}
	return leaders
}

func leaseHolderIdentity(kubeconfigPath, namespace, name string) (string, error) {
	output, err := runKubectl(kubeconfigPath, "get", "lease", name, "-n", namespace, "-o", "json")
	if err != nil {
		return "", err
	}

	var lease struct {
		Spec struct {
			HolderIdentity string `json:"holderIdentity"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(output), &lease); err != nil {
		return "", fmt.Errorf("failed to parse %s/%s lease: %w", namespace, name, err)
	}

	return strings.TrimSpace(lease.Spec.HolderIdentity), nil
}

func humanDurationSince(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	d := time.Since(ts)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func runKubectl(kubeconfigPath string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfigPath, "--request-timeout=5s"}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kubectl %s failed: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func readTerraformFlatOutputs(repoRoot string) (map[string]string, error) {
	cmd := exec.Command("terraform", "output", "-no-color", "-json", "flat_outputs")
	cmd.Dir = filepath.Join(repoRoot, "modules", "aws")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("terraform output failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}

	var outputs map[string]string
	if err := json.Unmarshal(output, &outputs); err != nil {
		return nil, fmt.Errorf("failed to parse terraform outputs: %w", err)
	}
	return outputs, nil
}

func readRequestedRancherVersionsForPanel(totalHAs int) []string {
	versions := viper.GetStringSlice("rancher.versions")
	if len(versions) == totalHAs {
		out := make([]string, 0, len(versions))
		for _, version := range versions {
			out = append(out, normalizeVersionInput(version))
		}
		return out
	}

	version := normalizeVersionInput(viper.GetString("rancher.version"))
	if version == "" {
		return nil
	}
	if totalHAs == 1 {
		return []string{version}
	}

	out := make([]string, totalHAs)
	for i := range out {
		out[i] = version
	}
	return out
}

func (p *localControlPanel) startCleanup() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cleanupRunning {
		return fmt.Errorf("cleanup is already running")
	}

	now := time.Now()
	p.cleanupRunning = true
	p.cleanupStartedAt = &now
	p.cleanupFinishedAt = nil
	p.cleanupError = ""
	p.cleanupOutput = []string{"[control-panel] Starting canonical cleanup via go test -run TestHACleanup"}

	go p.runCleanupCommand()
	return nil
}

func (p *localControlPanel) runCleanupCommand() {
	cmd := exec.Command("go", "test", "-v", "-run", "TestHACleanup", "-timeout", "20m", "./terratest")
	cmd.Dir = p.repoRoot

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		p.finishCleanup(fmt.Errorf("failed to capture cleanup output: %w", err))
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		p.finishCleanup(fmt.Errorf("failed to capture cleanup output: %w", err))
		return
	}

	if err := cmd.Start(); err != nil {
		p.finishCleanup(fmt.Errorf("failed to start cleanup command: %w", err))
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go p.captureCleanupStream(&wg, stdout)
	go p.captureCleanupStream(&wg, stderr)
	wg.Wait()

	p.finishCleanup(cmd.Wait())
}

func (p *localControlPanel) captureCleanupStream(wg *sync.WaitGroup, reader io.Reader) {
	defer wg.Done()
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		p.appendCleanupOutput(scanner.Text())
	}
}

func (p *localControlPanel) appendCleanupOutput(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.cleanupOutput = append(p.cleanupOutput, line)
	if len(p.cleanupOutput) > 500 {
		p.cleanupOutput = append([]string(nil), p.cleanupOutput[len(p.cleanupOutput)-500:]...)
	}
}

func (p *localControlPanel) finishCleanup(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.cleanupRunning = false
	finishedAt := time.Now()
	p.cleanupFinishedAt = &finishedAt
	if err != nil {
		p.cleanupError = err.Error()
		p.cleanupOutput = append(p.cleanupOutput, "[control-panel] Cleanup finished with error: "+err.Error())
		return
	}

	p.cleanupError = ""
	p.cleanupOutput = append(p.cleanupOutput, "[control-panel] Cleanup completed successfully")
}

func (p *localControlPanel) logRequest(r *http.Request) (clusterView, string, string, string, error) {
	clusterID := strings.TrimSpace(r.URL.Query().Get("cluster"))
	pod := strings.TrimSpace(r.URL.Query().Get("pod"))
	namespace := strings.TrimSpace(r.URL.Query().Get("namespace"))
	container := strings.TrimSpace(r.URL.Query().Get("container"))
	if clusterID == "" || pod == "" {
		return clusterView{}, "", "", "", fmt.Errorf("cluster and pod are required")
	}
	if namespace == "" {
		namespace = "cattle-system"
	}

	for _, cluster := range p.discoverClusters() {
		if cluster.ID == clusterID {
			if !cluster.Available {
				return clusterView{}, "", "", "", fmt.Errorf("cluster is not available")
			}
			if !cluster.Reachable {
				return clusterView{}, "", "", "", fmt.Errorf("cluster is not reachable")
			}
			return cluster, pod, namespace, container, nil
		}
	}

	return clusterView{}, "", "", "", fmt.Errorf("cluster %s not found", clusterID)
}

func (p *localControlPanel) clusterByID(clusterID string) (clusterView, error) {
	for _, cluster := range p.discoverClusters() {
		if cluster.ID == clusterID {
			return cluster, nil
		}
	}
	return clusterView{}, fmt.Errorf("cluster %s not found", clusterID)
}

func (p *localControlPanel) kubeconfigContentForDownload(cluster clusterView) ([]byte, string, error) {
	filename := strings.TrimSpace(cluster.DownloadName)
	if filename == "" {
		filename = "kubeconfig.yaml"
	}

	switch cluster.Type {
	case "local":
		if cluster.KubeconfigPath == "" {
			return nil, "", fmt.Errorf("local kubeconfig path is unavailable")
		}
		data, err := os.ReadFile(cluster.KubeconfigPath)
		if err != nil {
			return nil, "", fmt.Errorf("failed to read local kubeconfig: %w", err)
		}
		return data, filename, nil
	case "downstream":
		if cluster.ManagementClusterID == "" {
			return nil, "", fmt.Errorf("downstream cluster has no management cluster id yet")
		}
		token, err := p.rancherToken(cluster.HAIndex, cluster.RancherURL)
		if err != nil {
			return nil, "", err
		}
		kubeconfig, err := generateRancherKubeconfig(cluster.RancherURL, token, cluster.ManagementClusterID)
		if err != nil {
			return nil, "", err
		}
		return []byte(kubeconfig), filename, nil
	default:
		return nil, "", fmt.Errorf("unsupported cluster type %q", cluster.Type)
	}
}

func (p *localControlPanel) ensureDownstreamKubeconfig(haIndex int, rancherURL, clusterKey, managementClusterID, existingPath string) (string, error) {
	if existingPath != "" {
		if _, err := os.Stat(existingPath); err == nil {
			return existingPath, nil
		}
	}

	p.mu.Lock()
	if path := p.downstreamKubeconfigCache[clusterKey]; path != "" {
		if _, err := os.Stat(path); err == nil {
			p.mu.Unlock()
			return path, nil
		}
	}
	p.mu.Unlock()

	token, err := p.rancherToken(haIndex, rancherURL)
	if err != nil {
		return "", err
	}
	kubeconfig, err := generateRancherKubeconfig(rancherURL, token, managementClusterID)
	if err != nil {
		return "", err
	}

	cacheDir := filepath.Join(automationOutputDir(), "control-panel")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(cacheDir, clusterKey+".yaml")
	if err := os.WriteFile(path, []byte(kubeconfig), 0o600); err != nil {
		return "", err
	}

	p.mu.Lock()
	p.downstreamKubeconfigCache[clusterKey] = path
	p.mu.Unlock()
	return path, nil
}

func (p *localControlPanel) rancherToken(haIndex int, rancherURL string) (string, error) {
	p.mu.Lock()
	if token := p.rancherTokens[haIndex]; token != "" {
		p.mu.Unlock()
		return token, nil
	}
	p.mu.Unlock()

	token, err := createRancherAdminToken(rancherURL, viper.GetString("rancher.bootstrap_password"))
	if err != nil {
		return "", err
	}

	p.mu.Lock()
	p.rancherTokens[haIndex] = token
	p.mu.Unlock()
	return token, nil
}

func (p *localControlPanel) authorized(r *http.Request) bool {
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Control-Panel-Token"))
	}
	return token != "" && token == p.token
}

func (p *localControlPanel) authorizedReadOnly(r *http.Request) bool {
	return p.authorized(r) || requestFromLoopback(r)
}

func (p *localControlPanel) authorizedLocalBrowserRead(r *http.Request) bool {
	return p.authorized(r) || (requestFromLoopback(r) && sameOriginBrowserRequest(r))
}

func (p *localControlPanel) authorizedLocalAction(r *http.Request) bool {
	return p.authorized(r) || (requestFromLoopback(r) && sameOriginBrowserRequest(r))
}

func requestFromLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sameOriginBrowserRequest(r *http.Request) bool {
	if !sameOriginHeaderHost(r.Header.Get("Origin"), r.Host) {
		return sameOriginHeaderHost(r.Header.Get("Referer"), r.Host)
	}
	return true
}

func sameOriginHeaderHost(rawValue, requestHost string) bool {
	rawValue = strings.TrimSpace(rawValue)
	if rawValue == "" {
		return false
	}

	u, err := url.Parse(rawValue)
	if err != nil {
		return false
	}

	return strings.EqualFold(u.Host, requestHost)
}

func resolveControlPanelPaths(startDir string) (repoRoot string, testDir string, err error) {
	current := filepath.Clean(startDir)
	for {
		goModPath := filepath.Join(current, "go.mod")
		terratestDir := filepath.Join(current, "terratest")
		if fileExists(goModPath) && dirExists(terratestDir) {
			return current, terratestDir, nil
		}

		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}

	return "", "", fmt.Errorf("failed to locate repository root from %s", startDir)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func writeJSON(w http.ResponseWriter, value interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(value)
}
