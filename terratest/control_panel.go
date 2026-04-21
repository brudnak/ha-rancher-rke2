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
}

type panelState struct {
	Clusters panelClusterState `json:"clusters"`
	Cleanup  cleanupState      `json:"cleanup"`
}

type panelClusterState struct {
	Items []clusterView `json:"items"`
}

type clusterView struct {
	ID             int       `json:"id"`
	Name           string    `json:"name"`
	Version        string    `json:"version,omitempty"`
	RancherURL     string    `json:"rancherUrl,omitempty"`
	LoadBalancer   string    `json:"loadBalancer,omitempty"`
	KubeconfigPath string    `json:"kubeconfigPath,omitempty"`
	Available      bool      `json:"available"`
	Reachable      bool      `json:"reachable"`
	Error          string    `json:"error,omitempty"`
	Pods           []podView `json:"pods"`
}

type podView struct {
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
		token:    token,
		totalHAs: totalHAs,
		repoRoot: repoRoot,
		testDir:  testDir,
		listener: listener,
		baseURL:  fmt.Sprintf("http://%s/?token=%s", listener.Addr().String(), token),
		doneCh:   make(chan error, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", panel.handleIndex)
	mux.HandleFunc("/api/state", panel.handleState)
	mux.HandleFunc("/api/logs", panel.handleLogs)
	mux.HandleFunc("/api/logs/stream", panel.handleLogStream)
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

	page := template.Must(template.New("control-panel").Parse(controlPanelHTML))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = page.Execute(w, struct {
		Token string
	}{
		Token: p.token,
	})
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

	cluster, pod, container, err := p.logRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	args := []string{"logs", pod, "-n", "cattle-system", "--tail=200"}
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

	cluster, pod, container, err := p.logRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	args := []string{"logs", "-f", pod, "-n", "cattle-system", "--tail=20"}
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

	clusters := make([]clusterView, 0, p.totalHAs)
	for i := 1; i <= p.totalHAs; i++ {
		cluster := clusterView{
			ID:   i,
			Name: fmt.Sprintf("HA %d", i),
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
		pods, err := fetchRelevantPods(cluster.KubeconfigPath)
		if err != nil {
			cluster.Error = err.Error()
			clusters = append(clusters, cluster)
			continue
		}

		cluster.Reachable = true
		cluster.Pods = pods
		clusters = append(clusters, cluster)
	}

	return clusters
}

func fetchRelevantPods(kubeconfigPath string) ([]podView, error) {
	output, err := runKubectl(kubeconfigPath, "get", "pods", "-n", "cattle-system", "-o", "json")
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
		nameLower := strings.ToLower(item.Metadata.Name)
		if !strings.Contains(nameLower, "rancher") && !strings.Contains(nameLower, "webhook") {
			continue
		}

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

func (p *localControlPanel) logRequest(r *http.Request) (clusterView, string, string, error) {
	clusterID := strings.TrimSpace(r.URL.Query().Get("cluster"))
	pod := strings.TrimSpace(r.URL.Query().Get("pod"))
	container := strings.TrimSpace(r.URL.Query().Get("container"))
	if clusterID == "" || pod == "" {
		return clusterView{}, "", "", fmt.Errorf("cluster and pod are required")
	}

	for _, cluster := range p.discoverClusters() {
		if fmt.Sprintf("%d", cluster.ID) == clusterID {
			if !cluster.Available {
				return clusterView{}, "", "", fmt.Errorf("cluster is not available")
			}
			if !cluster.Reachable {
				return clusterView{}, "", "", fmt.Errorf("cluster is not reachable")
			}
			return cluster, pod, container, nil
		}
	}

	return clusterView{}, "", "", fmt.Errorf("cluster %s not found", clusterID)
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

const controlPanelHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Rancher Local Control Panel</title>
  <style>
    :root {
      color-scheme: light dark;
      --bg: #f2efe8;
      --panel: rgba(255, 252, 248, 0.96);
      --text: #1f1b17;
      --muted: #6b6258;
      --border: rgba(88, 71, 49, 0.16);
      --accent: #175f49;
      --accent-strong: #0f4636;
      --danger: #9a3424;
      --warning: #a06b00;
      --shadow: 0 28px 80px rgba(38, 29, 17, 0.16);
    }

    @media (prefers-color-scheme: dark) {
      :root {
        --bg: #171b19;
        --panel: rgba(30, 35, 33, 0.96);
        --text: #f3efe8;
        --muted: #b5ab9f;
        --border: rgba(213, 201, 185, 0.14);
        --accent: #56bb95;
        --accent-strong: #3ca37d;
        --danger: #ff8e7d;
        --warning: #e4b44d;
        --shadow: 0 28px 80px rgba(0, 0, 0, 0.42);
      }
    }

    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: ui-sans-serif, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      color: var(--text);
      background:
        radial-gradient(circle at top left, rgba(44, 145, 112, 0.12), transparent 28%),
        radial-gradient(circle at top right, rgba(198, 145, 42, 0.1), transparent 24%),
        var(--bg);
      min-height: 100vh;
    }
    .shell {
      width: min(1500px, calc(100vw - 32px));
      margin: 20px auto 28px;
      display: grid;
      gap: 18px;
    }
    .hero, .panel {
      background: var(--panel);
      border: 1px solid var(--border);
      border-radius: 22px;
      box-shadow: var(--shadow);
      backdrop-filter: blur(16px);
    }
    .hero {
      padding: 24px 28px;
      display: flex;
      justify-content: space-between;
      gap: 20px;
      align-items: flex-start;
    }
    .hero h1 {
      margin: 0;
      font-size: clamp(1.6rem, 2vw, 2.3rem);
      line-height: 1.05;
    }
    .hero p {
      margin: 10px 0 0;
      color: var(--muted);
      max-width: 72ch;
    }
    .actions {
      display: flex;
      gap: 10px;
      flex-wrap: wrap;
      justify-content: flex-end;
    }
    button {
      appearance: none;
      border: 0;
      border-radius: 999px;
      padding: 11px 16px;
      font: inherit;
      font-weight: 700;
      cursor: pointer;
      transition: transform 120ms ease, background 120ms ease, opacity 120ms ease;
    }
    button:hover { transform: translateY(-1px); }
    button:active { transform: translateY(0); }
    .secondary { background: rgba(127, 111, 92, 0.14); color: var(--text); }
    .primary { background: var(--accent); color: #fff; }
    .primary:hover { background: var(--accent-strong); }
    .danger { background: rgba(154, 52, 36, 0.16); color: var(--danger); }
    .layout {
      display: grid;
      grid-template-columns: minmax(0, 1.8fr) minmax(360px, 0.95fr);
      gap: 18px;
    }
    .left, .right {
      display: grid;
      gap: 18px;
      align-content: start;
    }
    .panel {
      padding: 18px 20px 20px;
    }
    .panel h2 {
      margin: 0 0 12px;
      font-size: 1.05rem;
    }
    .cluster-grid {
      display: grid;
      gap: 14px;
    }
    .cluster-card {
      border: 1px solid var(--border);
      border-radius: 16px;
      padding: 16px;
      background: rgba(0, 0, 0, 0.02);
    }
    .cluster-top {
      display: flex;
      justify-content: space-between;
      gap: 12px;
      align-items: center;
      margin-bottom: 10px;
    }
    .cluster-name {
      font-weight: 800;
      font-size: 1rem;
    }
    .pill {
      border-radius: 999px;
      padding: 6px 10px;
      font-size: 0.8rem;
      font-weight: 700;
    }
    .ok { background: rgba(29, 140, 106, 0.14); color: var(--accent); }
    .warn { background: rgba(160, 107, 0, 0.14); color: var(--warning); }
    .bad { background: rgba(154, 52, 36, 0.14); color: var(--danger); }
    .meta {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
      gap: 10px;
      margin-bottom: 14px;
      color: var(--muted);
      font-size: 0.92rem;
    }
    .meta > div { min-width: 0; }
    .meta strong { color: var(--text); display: block; margin-bottom: 2px; }
    .meta-value {
      display: block;
      overflow-wrap: anywhere;
      word-break: break-word;
    }
    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 0.93rem;
    }
    th, td {
      text-align: left;
      padding: 10px 8px;
      border-bottom: 1px solid var(--border);
      vertical-align: top;
    }
    th { color: var(--muted); font-size: 0.8rem; text-transform: uppercase; letter-spacing: 0.04em; }
    .pod-actions {
      display: flex;
      gap: 8px;
      flex-wrap: wrap;
    }
    .tiny {
      padding: 7px 10px;
      border-radius: 999px;
      background: rgba(127, 111, 92, 0.14);
      color: var(--text);
      font-size: 0.8rem;
    }
    .pod-name {
      display: flex;
      align-items: center;
      gap: 8px;
      flex-wrap: wrap;
    }
    .badge {
      display: inline-flex;
      align-items: center;
      border-radius: 999px;
      padding: 3px 8px;
      font-size: 0.72rem;
      font-weight: 800;
      letter-spacing: 0.02em;
      background: rgba(29, 140, 106, 0.14);
      color: var(--accent);
      white-space: nowrap;
    }
    .leader-summary {
      margin: 0 0 12px;
      color: var(--muted);
      font-size: 0.9rem;
    }
    .leader-summary strong {
      color: var(--text);
    }
    .leader-row {
      background: rgba(29, 140, 106, 0.06);
    }
    .leader-row.leader-changed {
      animation: leaderPulse 1.2s ease-in-out 3;
      box-shadow: inset 0 0 0 1px rgba(29, 140, 106, 0.26);
    }
    @keyframes leaderPulse {
      0% { background: rgba(29, 140, 106, 0.08); }
      35% { background: rgba(29, 140, 106, 0.24); }
      100% { background: rgba(29, 140, 106, 0.06); }
    }
    .logbox, .cleanup-box {
      min-height: 260px;
      max-height: 42vh;
      overflow: auto;
      border: 1px solid var(--border);
      border-radius: 16px;
      background: rgba(0, 0, 0, 0.06);
      padding: 14px 16px;
      font: 12px/1.55 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
      white-space: pre-wrap;
      word-break: break-word;
    }
    .muted { color: var(--muted); }
    .toolbar {
      display: flex;
      gap: 10px;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 12px;
      flex-wrap: wrap;
    }
    .statusline {
      color: var(--muted);
      font-size: 0.9rem;
    }
    .confirm-row {
      display: flex;
      gap: 10px;
      flex-wrap: wrap;
      align-items: center;
      margin-bottom: 12px;
    }
    input[type="text"] {
      border: 1px solid var(--border);
      background: transparent;
      color: var(--text);
      border-radius: 999px;
      padding: 10px 14px;
      min-width: 220px;
      font: inherit;
    }
    a { color: var(--accent); text-decoration: none; }
    a:hover { text-decoration: underline; }
    @media (max-width: 1100px) {
      .layout { grid-template-columns: 1fr; }
      .hero { flex-direction: column; }
      .actions { justify-content: flex-start; }
    }
  </style>
</head>
<body>
  <div class="shell">
    <section class="hero">
      <div>
        <h1>Rancher Local Control Panel</h1>
        <p>Local-only viewer for active HA Rancher runs. It focuses on <code>cattle-system</code>, especially Rancher and Rancher webhook pods, and can call the existing full cleanup flow with typed confirmation.</p>
      </div>
      <div class="actions">
        <button class="secondary" id="refreshBtn">Refresh</button>
        <button class="secondary" id="stopBtn">Stop Panel</button>
      </div>
    </section>

    <section class="layout">
      <div class="left">
        <section class="panel">
          <div class="toolbar">
            <h2>Clusters</h2>
            <div class="statusline" id="refreshStatus">Refreshing…</div>
          </div>
          <div class="cluster-grid" id="clusters"></div>
        </section>
      </div>
      <div class="right">
        <section class="panel">
          <div class="toolbar">
            <h2>Logs</h2>
            <div class="pod-actions">
              <button class="secondary tiny" id="stopStreamBtn">Stop Live</button>
              <button class="secondary tiny" id="clearLogsBtn">Clear</button>
            </div>
          </div>
          <div class="statusline" id="logStatus">Select a Rancher or webhook pod to view logs.</div>
          <div class="logbox" id="logBox"></div>
        </section>

        <section class="panel">
          <div class="toolbar">
            <h2>Cleanup</h2>
            <span class="statusline">Calls the existing full cleanup flow.</span>
          </div>
          <div class="confirm-row">
            <input type="text" id="cleanupConfirm" placeholder='Type "cleanup" to enable' />
            <button class="danger" id="cleanupBtn">Run Cleanup</button>
          </div>
          <div class="statusline" id="cleanupStatus">Idle</div>
          <div class="cleanup-box" id="cleanupBox"></div>
        </section>
      </div>
    </section>
  </div>

  <script>
    const token = {{printf "%q" .Token}};
    const clustersEl = document.getElementById('clusters');
    const refreshStatusEl = document.getElementById('refreshStatus');
    const logStatusEl = document.getElementById('logStatus');
    const logBoxEl = document.getElementById('logBox');
    const cleanupStatusEl = document.getElementById('cleanupStatus');
    const cleanupBoxEl = document.getElementById('cleanupBox');
    const cleanupConfirmEl = document.getElementById('cleanupConfirm');
    let stream = null;
    let previousLeaders = new Map();
    let pendingLeaderHighlights = new Map();
    let lastLeaderChangeMessage = '';

    function escapeHtml(value) {
      return value
        .replaceAll('&', '&amp;')
        .replaceAll('<', '&lt;')
        .replaceAll('>', '&gt;');
    }

    async function fetchState() {
      const response = await fetch('/api/state?token=' + encodeURIComponent(token));
      if (!response.ok) {
        throw new Error('Failed to fetch state');
      }
      return response.json();
    }

    function renderClusters(state) {
      const clusterItems = state && state.clusters && Array.isArray(state.clusters.items) ? state.clusters.items : [];
      if (!clusterItems.length) {
        clustersEl.innerHTML = '<div class="cluster-card muted">No clusters discovered yet.</div>';
        return;
      }

      clustersEl.innerHTML = clusterItems.map(cluster => {
        const statusClass = cluster.reachable ? 'ok' : (cluster.available ? 'warn' : 'bad');
        const statusText = cluster.reachable ? 'Reachable' : (cluster.available ? 'Unavailable' : 'Missing');
        const pods = Array.isArray(cluster.pods) ? cluster.pods : [];
        const currentLeader = pods.find(pod => pod.leader && pod.leaderLabel === 'Leader') || pods.find(pod => pod.leader);
        const changedLeader = pendingLeaderHighlights.get(cluster.id);
        const podRows = pods.length ? pods.map(pod => {
          const quotedPodName = JSON.stringify(pod.name);
          const leaderBadge = pod.leader && pod.leaderLabel
            ? '<span class="badge">' + escapeHtml(pod.leaderLabel) + '</span>'
            : '';
          const rowClass = changedLeader && changedLeader === pod.name ? ' class="leader-row leader-changed"' : (pod.leader ? ' class="leader-row"' : '');
          return (
          '<tr' + rowClass + '>' +
            '<td><div class="pod-name"><span>' + escapeHtml(pod.name) + '</span>' + leaderBadge + '</div></td>' +
            '<td>' + escapeHtml(pod.ready) + '</td>' +
            '<td>' + escapeHtml(pod.status) + '</td>' +
            '<td>' + pod.restarts + '</td>' +
            '<td>' + escapeHtml(pod.age) + '</td>' +
            '<td>' + escapeHtml(pod.node || '') + '</td>' +
            '<td>' + escapeHtml(pod.containers) + '</td>' +
            '<td><div class="pod-actions">' +
              "<button class=\"tiny\" onclick='loadLogs(" + cluster.id + ", " + quotedPodName + ")'>Tail</button>" +
              "<button class=\"tiny\" onclick='streamLogs(" + cluster.id + ", " + quotedPodName + ")'>Live</button>" +
            '</div></td>' +
          '</tr>'
          );
        }).join('') : '<tr><td colspan="8" class="muted">' + (cluster.error ? escapeHtml(cluster.error) : 'No Rancher/webhook pods found in cattle-system.') + '</td></tr>';

        const rancherLink = cluster.rancherUrl ? '<a href="' + cluster.rancherUrl + '" target="_blank" rel="noreferrer">' + cluster.rancherUrl + '</a>' : '<span class="muted">Unavailable</span>';
        const versionSuffix = cluster.version ? ' <span class="muted">(' + escapeHtml(cluster.version) + ')</span>' : '';
        const loadBalancer = cluster.loadBalancer ? escapeHtml(cluster.loadBalancer) : '<span class="muted">Unavailable</span>';
        const kubeconfigPath = cluster.kubeconfigPath ? escapeHtml(cluster.kubeconfigPath) : '<span class="muted">Unavailable</span>';
        const leaderSummary = currentLeader
          ? '<div class="leader-summary"><strong>Active Leader</strong> ' + escapeHtml(currentLeader.name) + '</div>'
          : '<div class="leader-summary muted">Leader not detected yet.</div>';

        return '<div class="cluster-card">' +
          '<div class="cluster-top">' +
            '<div class="cluster-name">' + escapeHtml(cluster.name) + versionSuffix + '</div>' +
            '<span class="pill ' + statusClass + '">' + statusText + '</span>' +
          '</div>' +
          '<div class="meta">' +
            '<div><strong>Rancher URL</strong><span class="meta-value">' + rancherLink + '</span></div>' +
            '<div><strong>Load Balancer</strong><span class="meta-value">' + loadBalancer + '</span></div>' +
            '<div><strong>Kubeconfig</strong><span class="meta-value">' + kubeconfigPath + '</span></div>' +
          '</div>' +
          leaderSummary +
          '<table>' +
            '<thead><tr>' +
              '<th>Pod</th>' +
              '<th>Ready</th>' +
              '<th>Status</th>' +
              '<th>Restarts</th>' +
              '<th>Age</th>' +
              '<th>Node</th>' +
              '<th>Containers</th>' +
              '<th>Logs</th>' +
            '</tr></thead>' +
            '<tbody>' + podRows + '</tbody>' +
          '</table>' +
        '</div>';
      }).join('');
    }

    function updateLeaderTracking(state) {
      const messages = [];
      const nextLeaders = new Map();
      const clusterItems = state && state.clusters && Array.isArray(state.clusters.items) ? state.clusters.items : [];

      clusterItems.forEach(cluster => {
        const pods = Array.isArray(cluster.pods) ? cluster.pods : [];
        const currentLeader = pods.find(pod => pod.leader && pod.leaderLabel === 'Leader') || pods.find(pod => pod.leader);
        const currentLeaderName = currentLeader ? currentLeader.name : '';
        const previousLeaderName = previousLeaders.get(cluster.id) || '';

        if (currentLeaderName) {
          nextLeaders.set(cluster.id, currentLeaderName);
        }

        if (currentLeaderName && previousLeaderName && previousLeaderName !== currentLeaderName) {
          pendingLeaderHighlights.set(cluster.id, currentLeaderName);
          window.setTimeout(() => {
            if (pendingLeaderHighlights.get(cluster.id) === currentLeaderName) {
              pendingLeaderHighlights.delete(cluster.id);
            }
          }, 4500);
          messages.push(cluster.name + ' leader changed to ' + currentLeaderName);
        }
      });

      previousLeaders = nextLeaders;
      lastLeaderChangeMessage = messages.join(' • ');
    }

    function renderCleanup(cleanup) {
      const cleanupOutput = cleanup && Array.isArray(cleanup.output) ? cleanup.output : [];
      if (cleanup.running) {
        cleanupStatusEl.textContent = 'Cleanup running' + (cleanup.startedAt ? ' since ' + new Date(cleanup.startedAt).toLocaleTimeString() : '');
      } else if (cleanup.error) {
        cleanupStatusEl.textContent = 'Cleanup finished with error';
      } else if (cleanup.finishedAt) {
        cleanupStatusEl.textContent = 'Cleanup finished successfully at ' + new Date(cleanup.finishedAt).toLocaleTimeString();
      } else {
        cleanupStatusEl.textContent = 'Idle';
      }

      cleanupBoxEl.textContent = cleanupOutput.join('\n');
      cleanupBoxEl.scrollTop = cleanupBoxEl.scrollHeight;
    }

    let refreshInFlight = false;

    async function refresh() {
      if (refreshInFlight) {
        return;
      }
      refreshInFlight = true;
      refreshStatusEl.textContent = 'Refreshing…';
      try {
        const state = await fetchState();
        updateLeaderTracking(state);
        renderClusters(state);
        renderCleanup(state.cleanup);
        if (lastLeaderChangeMessage) {
          refreshStatusEl.textContent = lastLeaderChangeMessage + ' • ' + new Date().toLocaleTimeString();
        } else {
          refreshStatusEl.textContent = 'Last refreshed at ' + new Date().toLocaleTimeString();
        }
      } catch (error) {
        if (!clustersEl.innerHTML.trim()) {
          refreshStatusEl.textContent = error.message;
        } else {
          refreshStatusEl.textContent = 'Last refresh attempt failed: ' + error.message;
        }
      } finally {
        refreshInFlight = false;
      }
    }

    async function loadLogs(clusterId, podName) {
      stopStream();
      logStatusEl.textContent = 'Loading logs for ' + podName + '…';
      const response = await fetch('/api/logs?token=' + encodeURIComponent(token) + '&cluster=' + encodeURIComponent(clusterId) + '&pod=' + encodeURIComponent(podName));
      const raw = await response.text();
      let payload = {};
      try {
        payload = JSON.parse(raw);
      } catch (error) {
        payload = { text: raw };
      }
      if (!response.ok) {
        logStatusEl.textContent = payload.text || 'Failed to load logs';
        return;
      }
      logBoxEl.textContent = payload.text || '';
      logBoxEl.scrollTop = logBoxEl.scrollHeight;
      logStatusEl.textContent = 'Showing recent logs for ' + podName;
    }

    function stopStream() {
      if (stream) {
        stream.close();
        stream = null;
        logStatusEl.textContent = 'Live log stream stopped.';
      }
    }

    function streamLogs(clusterId, podName) {
      stopStream();
      logBoxEl.textContent = '';
      logStatusEl.textContent = 'Streaming live logs for ' + podName + '…';
      stream = new EventSource('/api/logs/stream?token=' + encodeURIComponent(token) + '&cluster=' + encodeURIComponent(clusterId) + '&pod=' + encodeURIComponent(podName));
      stream.addEventListener('line', event => {
        logBoxEl.textContent += event.data + '\n';
        logBoxEl.scrollTop = logBoxEl.scrollHeight;
      });
      stream.addEventListener('error', event => {
        if (event.data) {
          logBoxEl.textContent += '\n[error] ' + event.data + '\n';
        }
      });
      stream.addEventListener('end', () => {
        logStatusEl.textContent = 'Live stream finished for ' + podName;
        stopStream();
      });
    }

    async function runCleanup() {
      if (cleanupConfirmEl.value.trim().toLowerCase() !== 'cleanup') {
        cleanupStatusEl.textContent = 'Type cleanup to confirm.';
        return;
      }

      const response = await fetch('/api/cleanup?token=' + encodeURIComponent(token), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ confirm: cleanupConfirmEl.value.trim() })
      });
      if (!response.ok) {
        cleanupStatusEl.textContent = await response.text();
        return;
      }

      cleanupConfirmEl.value = '';
      cleanupStatusEl.textContent = 'Cleanup requested…';
      refresh();
    }

    async function stopPanel() {
      if (!confirm('Stop the local control panel?')) {
        return;
      }
      await fetch('/api/shutdown?token=' + encodeURIComponent(token), { method: 'POST' });
      setTimeout(() => window.close(), 250);
    }

    document.getElementById('refreshBtn').addEventListener('click', refresh);
    document.getElementById('cleanupBtn').addEventListener('click', runCleanup);
    document.getElementById('stopBtn').addEventListener('click', stopPanel);
    document.getElementById('stopStreamBtn').addEventListener('click', stopStream);
    document.getElementById('clearLogsBtn').addEventListener('click', () => {
      logBoxEl.textContent = '';
      logStatusEl.textContent = 'Logs cleared.';
    });

    refresh();
    setInterval(refresh, 5000);
  </script>
</body>
</html>`
