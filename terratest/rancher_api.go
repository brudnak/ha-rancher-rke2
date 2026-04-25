package test

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func createRancherAdminToken(rancherURL, bootstrapPassword string) (string, error) {
	rancherURL = strings.TrimRight(clickableURL(rancherURL), "/")
	bootstrapPassword = strings.TrimSpace(bootstrapPassword)
	if bootstrapPassword == "" {
		return "", fmt.Errorf("rancher.bootstrap_password must be set to create an admin token")
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	loginPayload := map[string]string{
		"description":  "ha-rancher-rke2-automation",
		"responseType": "token",
		"username":     "admin",
		"password":     bootstrapPassword,
	}
	var loginResp struct {
		Token string `json:"token"`
	}
	if err := postRancherJSON(client, rancherURL+"/v3-public/localProviders/local?action=login", "", loginPayload, &loginResp); err != nil {
		return "", err
	}
	if loginResp.Token == "" {
		return "", fmt.Errorf("Rancher login response did not include a token")
	}

	tokenPayload := map[string]interface{}{
		"type":        "token",
		"metadata":    struct{}{},
		"description": "ha-rancher-rke2 automation",
		"ttl":         7776000000,
	}
	var tokenResp struct {
		Token string `json:"token"`
	}
	if err := postRancherJSON(client, rancherURL+"/v3/tokens", loginResp.Token, tokenPayload, &tokenResp); err != nil {
		return "", err
	}
	if tokenResp.Token == "" {
		return "", fmt.Errorf("Rancher token response did not include a token")
	}
	maskGitHubActionsValue(tokenResp.Token)
	return tokenResp.Token, nil
}

func configureRancherServerURL(rancherURL, bearerToken string) error {
	rancherURL = strings.TrimRight(clickableURL(rancherURL), "/")
	if strings.TrimSpace(bearerToken) == "" {
		return fmt.Errorf("bearer token must not be empty")
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	payload := map[string]string{
		"name":  "server-url",
		"value": rancherURL,
	}
	return putRancherJSON(client, rancherURL+"/v3/settings/server-url", bearerToken, payload)
}

func generateRancherKubeconfig(rancherURL, bearerToken, clusterID string) (string, error) {
	rancherURL = strings.TrimRight(clickableURL(rancherURL), "/")
	clusterID = strings.TrimSpace(clusterID)
	if clusterID == "" {
		return "", fmt.Errorf("cluster id must not be empty")
	}
	if strings.TrimSpace(bearerToken) == "" {
		return "", fmt.Errorf("bearer token must not be empty")
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	var out struct {
		Config string `json:"config"`
	}
	apiURL := fmt.Sprintf("%s/v3/clusters/%s?action=generateKubeconfig", rancherURL, url.PathEscape(clusterID))
	if err := postRancherJSON(client, apiURL, bearerToken, map[string]string{}, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.Config) == "" {
		return "", fmt.Errorf("Rancher generateKubeconfig response did not include config for cluster %s", clusterID)
	}
	return out.Config, nil
}

func postRancherJSON(client *http.Client, url, bearerToken string, payload interface{}, out interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Rancher API POST %s returned HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("failed to parse Rancher API response from %s: %w", url, err)
	}
	return nil
}

func putRancherJSON(client *http.Client, url, bearerToken string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Rancher API PUT %s returned HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
