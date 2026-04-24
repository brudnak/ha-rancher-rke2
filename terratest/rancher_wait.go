package test

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
)

func TestHAWaitReady(t *testing.T) {
	requireExplicitLifecycleTest(t, "TestHAWaitReady")
	setupConfig(t)

	totalHAs := viper.GetInt("total_has")
	if totalHAs < 1 {
		t.Fatal("total_has must be at least 1")
	}

	terraformOptions := getTerraformOptions(t, totalHAs)
	outputs := getTerraformOutputs(t, terraformOptions)
	if len(outputs) == 0 {
		t.Fatal("No outputs received from terraform")
	}

	timeout := durationFromEnv("RANCHER_READY_TIMEOUT", 25*time.Minute)
	initialDelay := durationFromEnv("RANCHER_READY_INITIAL_DELAY", 30*time.Second)
	settleDelay := durationFromEnv("RANCHER_READY_SETTLE_DELAY", 30*time.Second)

	var wg sync.WaitGroup
	errCh := make(chan error, totalHAs)
	for i := 1; i <= totalHAs; i++ {
		instanceNum := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := waitForHAReady(instanceNum, outputs, timeout, initialDelay, settleDelay); err != nil {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)

	var failures []string
	for err := range errCh {
		failures = append(failures, err.Error())
	}
	if len(failures) > 0 {
		t.Fatalf("Rancher readiness failed:\n%s", strings.Join(failures, "\n"))
	}
}

func waitForHAReady(instanceNum int, outputs map[string]string, timeout, initialDelay, settleDelay time.Duration) error {
	haOutputs := getHAOutputs(instanceNum, outputs)
	rancherURL := clickableURL(haOutputs.RancherURL)
	kubeconfigPath := filepath.Join(fmt.Sprintf("high-availability-%d", instanceNum), "kube_config.yaml")

	log.Printf("[ready][ha-%d] Waiting for Rancher to become ready at %s", instanceNum, rancherURL)
	log.Printf("[ready][ha-%d] Kubeconfig: %s", instanceNum, kubeconfigPath)
	log.Printf("[ready][ha-%d] Initial delay: %s, timeout: %s", instanceNum, initialDelay, timeout)

	if initialDelay > 0 {
		time.Sleep(initialDelay)
	}

	client := rancherReadyHTTPClient()
	deadline := time.Now().Add(timeout)
	attempt := 0
	consecutiveReady := 0

	for time.Now().Before(deadline) {
		attempt++

		httpReady, httpSummary := rancherHTTPReady(client, rancherURL)
		podsReady, podsSummary, podsErr := rancherPodsReady(kubeconfigPath)
		if podsErr != nil {
			podsSummary = podsErr.Error()
		}

		if httpReady && podsReady {
			consecutiveReady++
			log.Printf("[ready][ha-%d] Attempt %d ready check passed (%d/2): http=%s pods=%s",
				instanceNum, attempt, consecutiveReady, httpSummary, podsSummary)
			if consecutiveReady >= 2 {
				if settleDelay > 0 {
					log.Printf("[ready][ha-%d] Rancher is ready; settling for %s before continuing", instanceNum, settleDelay)
					time.Sleep(settleDelay)
				}
				log.Printf("[ready][ha-%d] Rancher readiness confirmed", instanceNum)
				return nil
			}
		} else {
			consecutiveReady = 0
			log.Printf("[ready][ha-%d] Attempt %d not ready yet: http=%s pods=%s",
				instanceNum, attempt, httpSummary, podsSummary)
		}

		time.Sleep(20 * time.Second)
	}

	return fmt.Errorf("[ha-%d] timed out after %s waiting for Rancher readiness", instanceNum, timeout)
}

func rancherReadyHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

func rancherHTTPReady(client *http.Client, rancherURL string) (bool, string) {
	if rancherURL == "" {
		return false, "missing Rancher URL"
	}

	rootStatus, rootErr := rancherHTTPStatus(client, rancherURL)
	if rootErr != nil {
		return false, fmt.Sprintf("root error: %v", rootErr)
	}
	apiStatus, apiErr := rancherHTTPStatus(client, strings.TrimRight(rancherURL, "/")+"/v3")
	if apiErr != nil {
		return false, fmt.Sprintf("root=%d api error: %v", rootStatus, apiErr)
	}

	if rancherReadyStatus(rootStatus) && rancherReadyStatus(apiStatus) {
		return true, fmt.Sprintf("root=%d api=%d", rootStatus, apiStatus)
	}
	return false, fmt.Sprintf("root=%d api=%d", rootStatus, apiStatus)
}

func rancherHTTPStatus(client *http.Client, target string) (int, error) {
	resp, err := client.Get(target)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func rancherReadyStatus(status int) bool {
	switch status {
	case http.StatusOK,
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound:
		return true
	default:
		return false
	}
}

func rancherPodsReady(kubeconfigPath string) (bool, string, error) {
	if _, err := os.Stat(kubeconfigPath); err != nil {
		return false, "", fmt.Errorf("kubeconfig not available at %s: %w", kubeconfigPath, err)
	}

	pods, err := fetchRelevantPods(kubeconfigPath)
	if err != nil {
		return false, "", err
	}

	ready, summary := summarizeRancherPods(pods)
	return ready, summary, nil
}

func summarizeRancherPods(pods []podView) (bool, string) {
	seenRancher := false
	seenWebhook := false
	notReady := make([]string, 0)
	interesting := 0

	for _, pod := range pods {
		name := strings.ToLower(pod.Name)
		isWebhook := strings.Contains(name, "rancher-webhook")
		isRancher := strings.HasPrefix(name, "rancher-") && !isWebhook
		if !isRancher && !isWebhook {
			continue
		}

		interesting++
		if isWebhook {
			seenWebhook = true
		}
		if isRancher {
			seenRancher = true
		}
		if !podReadyForSignoff(pod) {
			notReady = append(notReady, fmt.Sprintf("%s ready=%s status=%s restarts=%d", pod.Name, pod.Ready, pod.Status, pod.Restarts))
		}
	}

	missing := make([]string, 0, 2)
	if !seenRancher {
		missing = append(missing, "rancher")
	}
	if !seenWebhook {
		missing = append(missing, "rancher-webhook")
	}

	switch {
	case len(missing) > 0:
		return false, fmt.Sprintf("waiting for pods: %s (found %d relevant pods)", strings.Join(missing, ", "), interesting)
	case len(notReady) > 0:
		return false, strings.Join(notReady, "; ")
	default:
		return true, fmt.Sprintf("rancher and rancher-webhook pods ready (%d relevant pods)", interesting)
	}
}

func podReadyForSignoff(pod podView) bool {
	parts := strings.Split(pod.Ready, "/")
	if len(parts) != 2 {
		return false
	}
	ready, readyErr := strconv.Atoi(parts[0])
	total, totalErr := strconv.Atoi(parts[1])
	if readyErr != nil || totalErr != nil || total == 0 || ready != total {
		return false
	}
	return strings.EqualFold(pod.Status, "Running")
}

func durationFromEnv(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		log.Printf("Invalid %s=%q, using default %s", name, value, fallback)
		return fallback
	}
	return duration
}
