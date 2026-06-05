package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

const providerName = "runpod"

type Config struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func NewClient(config Config) *Client {
	baseURL := strings.TrimRight(config.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://rest.runpod.io/v1"
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	return &Client{
		baseURL:    baseURL,
		apiKey:     config.APIKey,
		httpClient: httpClient,
	}
}

func (c *Client) Create(ctx context.Context, idempotencyKey string, request core.WorkloadRequest) (core.RemoteResource, error) {
	payload := createPodRequest{
		Name:             externalName(idempotencyKey),
		ComputeType:      "GPU",
		ImageName:        request.Image,
		DockerEntrypoint: append([]string(nil), request.Command...),
		DockerStartCmd:   append([]string(nil), request.Args...),
		Env:              cloneStringMap(request.Env),
		GPUCount:         request.GPUCount,
		GPUTypeIDs:       gpuTypeIDs(request.GPUType),
		Ports:            runpodPorts(request.Ports),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return core.RemoteResource{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/pods", bytes.NewReader(body))
	if err != nil {
		return core.RemoteResource{}, err
	}
	c.setHeaders(httpReq)

	var pod podResponse
	if err := c.doJSON(httpReq, http.StatusCreated, &pod); err != nil {
		return core.RemoteResource{}, err
	}
	return remoteFromPod(pod, idempotencyKey, request), nil
}

func (c *Client) Get(ctx context.Context, remoteID string) (core.RemoteResource, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/pods/"+remoteID, nil)
	if err != nil {
		return core.RemoteResource{}, err
	}
	c.setHeaders(httpReq)

	var pod podResponse
	if err := c.doJSON(httpReq, http.StatusOK, &pod); err != nil {
		return core.RemoteResource{}, err
	}
	return remoteFromPod(pod, "", core.WorkloadRequest{}), nil
}

func (c *Client) Delete(ctx context.Context, remoteID string) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/pods/"+remoteID, nil)
	if err != nil {
		return err
	}
	c.setHeaders(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("runpod: unexpected status %d for DELETE %s", resp.StatusCode, httpReq.URL.Path)
	}
}

func (c *Client) setHeaders(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
}

func (c *Client) doJSON(req *http.Request, expectedStatus int, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != expectedStatus {
		return fmt.Errorf("runpod: unexpected status %d for %s %s", resp.StatusCode, req.Method, req.URL.Path)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type createPodRequest struct {
	Name             string            `json:"name"`
	ComputeType      string            `json:"computeType"`
	ImageName        string            `json:"imageName"`
	DockerEntrypoint []string          `json:"dockerEntrypoint"`
	DockerStartCmd   []string          `json:"dockerStartCmd"`
	Env              map[string]string `json:"env,omitempty"`
	GPUCount         int               `json:"gpuCount"`
	GPUTypeIDs       []string          `json:"gpuTypeIds,omitempty"`
	Ports            []string          `json:"ports,omitempty"`
}

type podResponse struct {
	ID            string `json:"id"`
	DesiredStatus string `json:"desiredStatus"`
	Image         string `json:"image"`
	GPU           struct {
		Count       int    `json:"count"`
		DisplayName string `json:"displayName"`
	} `json:"gpu"`
}

func remoteFromPod(pod podResponse, idempotencyKey string, request core.WorkloadRequest) core.RemoteResource {
	return core.RemoteResource{
		ID:             pod.ID,
		IdempotencyKey: idempotencyKey,
		Provider:       providerName,
		Phase:          phaseFromDesiredStatus(pod.DesiredStatus),
		Request:        request,
	}
}

func phaseFromDesiredStatus(status string) core.RemotePhase {
	switch status {
	case "RUNNING":
		return core.RemoteRunning
	case "EXITED":
		return core.RemoteSucceeded
	case "TERMINATED":
		return core.RemoteDeleted
	default:
		return core.RemotePending
	}
}

func externalName(idempotencyKey string) string {
	name := strings.ToLower(idempotencyKey)
	replacer := strings.NewReplacer("/", "-", "_", "-", ".", "-", ":", "-")
	name = replacer.Replace(name)
	name = "gpu-burst-node-" + name
	if len(name) > 191 {
		return name[:191]
	}
	return name
}

func gpuTypeIDs(gpuType string) []string {
	if gpuType == "" {
		return nil
	}
	return []string{gpuType}
}

func runpodPorts(ports []core.Port) []string {
	if len(ports) == 0 {
		return nil
	}
	out := make([]string, 0, len(ports))
	for _, port := range ports {
		protocol := strings.ToLower(port.Protocol)
		if protocol == "" {
			protocol = "tcp"
		}
		out = append(out, fmt.Sprintf("%d/%s", port.ContainerPort, protocol))
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
