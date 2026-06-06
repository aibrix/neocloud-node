package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
	name := externalName(idempotencyKey)
	payload := createPodRequest{
		Name:             name,
		CloudType:        providerHint(request, core.AnnotationRunPodCloudType),
		ComputeType:      "GPU",
		ImageName:        request.Image,
		DockerEntrypoint: append([]string(nil), request.Command...),
		DockerStartCmd:   append([]string(nil), request.Args...),
		Env:              cloneStringMap(request.Env),
		GPUCount:         request.GPUCount,
		GPUTypeIDs:       gpuTypeIDs(request.GPUType),
		GPUTypePriority:  priorityForList(request.GPUType),
		Ports:            runpodPorts(request.Ports),
		Interruptible:    request.MarketType == core.MarketTypeSpot,
		TemplateID:       providerHint(request, core.AnnotationRunPodTemplateID),
		DataCenterIDs:    splitCSV(providerHint(request, core.AnnotationRunPodDataCenterIDs)),
		DataCenterPriority: providerHintOrDefault(request,
			core.AnnotationRunPodDataCenterPriority, priorityForList(providerHint(request, core.AnnotationRunPodDataCenterIDs))),
		NetworkVolumeID:         providerHint(request, core.AnnotationRunPodNetworkVolumeID),
		ContainerRegistryAuthID: providerHint(request, core.AnnotationRunPodRegistryAuthID),
		VolumeMountPath:         providerHint(request, core.AnnotationRunPodVolumeMountPath),
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
		if shouldRecoverCreate(err) {
			if existing, findErr := c.FindByExternalName(ctx, name); findErr == nil {
				existing.IdempotencyKey = idempotencyKey
				existing.Request = request
				return existing, nil
			}
		}
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

func (c *Client) List(ctx context.Context, filter PodFilter) ([]core.RemoteResource, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/pods", nil)
	if err != nil {
		return nil, err
	}
	query := httpReq.URL.Query()
	if filter.Name != "" {
		query.Set("name", filter.Name)
	}
	if filter.TemplateID != "" {
		query.Set("templateId", filter.TemplateID)
	}
	if filter.NetworkVolumeID != "" {
		query.Set("networkVolumeId", filter.NetworkVolumeID)
	}
	if filter.IncludeMachine {
		query.Set("includeMachine", "true")
	}
	if filter.IncludeNetworkVolume {
		query.Set("includeNetworkVolume", "true")
	}
	httpReq.URL.RawQuery = query.Encode()
	c.setHeaders(httpReq)

	var pods []podResponse
	if err := c.doJSON(httpReq, http.StatusOK, &pods); err != nil {
		return nil, err
	}

	resources := make([]core.RemoteResource, 0, len(pods))
	for _, pod := range pods {
		resources = append(resources, remoteFromPod(pod, "", core.WorkloadRequest{}))
	}
	return resources, nil
}

func (c *Client) ListRemoteResources(ctx context.Context) ([]core.RemoteResource, error) {
	return c.List(ctx, PodFilter{IncludeMachine: true, IncludeNetworkVolume: true})
}

func (c *Client) FindByExternalName(ctx context.Context, name string) (core.RemoteResource, error) {
	pods, err := c.List(ctx, PodFilter{Name: name, IncludeMachine: true, IncludeNetworkVolume: true})
	if err != nil {
		return core.RemoteResource{}, err
	}
	for _, pod := range pods {
		if pod.ID != "" {
			return pod, nil
		}
	}
	return core.RemoteResource{}, &core.ProviderError{
		Provider:  providerName,
		Operation: "list",
		Class:     core.ErrorClassRemoteNotFound,
		Message:   "pod not found by external name",
		Err:       core.ErrRemoteNotFound,
	}
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return providerHTTPError(httpReq, resp.StatusCode, body)
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
		return providerTransportError(req, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return providerTransportError(req, err)
	}
	if resp.StatusCode != expectedStatus {
		return providerHTTPError(req, resp.StatusCode, body)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(out); err != nil {
		return providerDecodeError(req, err)
	}
	return nil
}

type createPodRequest struct {
	Name                    string            `json:"name"`
	CloudType               string            `json:"cloudType,omitempty"`
	ComputeType             string            `json:"computeType"`
	ImageName               string            `json:"imageName"`
	DockerEntrypoint        []string          `json:"dockerEntrypoint"`
	DockerStartCmd          []string          `json:"dockerStartCmd"`
	Env                     map[string]string `json:"env,omitempty"`
	GPUCount                int               `json:"gpuCount"`
	GPUTypeIDs              []string          `json:"gpuTypeIds,omitempty"`
	GPUTypePriority         string            `json:"gpuTypePriority,omitempty"`
	Ports                   []string          `json:"ports,omitempty"`
	Interruptible           bool              `json:"interruptible,omitempty"`
	TemplateID              string            `json:"templateId,omitempty"`
	DataCenterIDs           []string          `json:"dataCenterIds,omitempty"`
	DataCenterPriority      string            `json:"dataCenterPriority,omitempty"`
	NetworkVolumeID         string            `json:"networkVolumeId,omitempty"`
	ContainerRegistryAuthID string            `json:"containerRegistryAuthId,omitempty"`
	VolumeMountPath         string            `json:"volumeMountPath,omitempty"`
}

type podResponse struct {
	ID                string         `json:"id"`
	Name              string         `json:"name"`
	DesiredStatus     string         `json:"desiredStatus"`
	Image             string         `json:"image"`
	TemplateID        string         `json:"templateId"`
	CostPerHr         costValue      `json:"costPerHr"`
	AdjustedCostPerHr float64        `json:"adjustedCostPerHr"`
	Ports             []string       `json:"ports"`
	PublicIP          string         `json:"publicIp"`
	PortMappings      map[string]int `json:"portMappings"`
	GPU               struct {
		Count       int    `json:"count"`
		DisplayName string `json:"displayName"`
	} `json:"gpu"`
	Machine struct {
		DataCenterID string `json:"dataCenterId"`
	} `json:"machine"`
	NetworkVolume struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Size         int    `json:"size"`
		DataCenterID string `json:"dataCenterId"`
	} `json:"networkVolume"`
}

type costValue struct {
	value float64
	set   bool
}

func (c *costValue) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	switch value := raw.(type) {
	case float64:
		c.value = value
		c.set = true
		return nil
	case string:
		if strings.TrimSpace(value) == "" {
			return nil
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return err
		}
		c.value = parsed
		c.set = true
		return nil
	default:
		return fmt.Errorf("unsupported costPerHr value %T", raw)
	}
}

type PodFilter struct {
	Name                 string
	TemplateID           string
	NetworkVolumeID      string
	IncludeMachine       bool
	IncludeNetworkVolume bool
}

func remoteFromPod(pod podResponse, idempotencyKey string, request core.WorkloadRequest) core.RemoteResource {
	return core.RemoteResource{
		ID:             pod.ID,
		IdempotencyKey: idempotencyKey,
		ExternalName:   pod.Name,
		Provider:       providerName,
		Phase:          phaseFromDesiredStatus(pod.DesiredStatus),
		Request:        request,
		HourlyCost:     hourlyCost(pod),
		CostSource:     costSource(pod),
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

func hourlyCost(pod podResponse) float64 {
	if pod.AdjustedCostPerHr > 0 {
		return pod.AdjustedCostPerHr
	}
	if pod.CostPerHr.set {
		return pod.CostPerHr.value
	}
	return 0
}

func costSource(pod podResponse) string {
	if pod.AdjustedCostPerHr > 0 {
		return "runpod.adjustedCostPerHr"
	}
	if pod.CostPerHr.set {
		return "runpod.costPerHr"
	}
	return ""
}

func externalName(idempotencyKey string) string {
	name := core.ManagedExternalName(idempotencyKey)
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

func priorityForList(raw string) string {
	if strings.Contains(raw, ",") {
		return "custom"
	}
	return ""
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

func providerHint(request core.WorkloadRequest, key string) string {
	if len(request.ProviderHints) == 0 {
		return ""
	}
	return strings.TrimSpace(request.ProviderHints[key])
}

func providerHintOrDefault(request core.WorkloadRequest, key string, fallback string) string {
	value := providerHint(request, key)
	if value != "" {
		return value
	}
	return fallback
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value != "" {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func shouldRecoverCreate(err error) bool {
	class := core.ClassifyError(err)
	return class == core.ErrorClassTransient || class == core.ErrorClassTimeout
}

func providerTransportError(req *http.Request, err error) error {
	return &core.ProviderError{
		Provider:  providerName,
		Operation: strings.ToLower(req.Method),
		Class:     core.ClassifyError(err),
		Message:   err.Error(),
		Err:       err,
	}
}

func providerDecodeError(req *http.Request, err error) error {
	return &core.ProviderError{
		Provider:  providerName,
		Operation: strings.ToLower(req.Method),
		Class:     core.ErrorClassTransient,
		Message:   "decode provider response: " + err.Error(),
		Err:       err,
	}
}

func providerHTTPError(req *http.Request, statusCode int, body []byte) error {
	class := classifyHTTPStatus(statusCode, string(body))
	err := fmt.Errorf("unexpected status %d for %s %s", statusCode, req.Method, req.URL.Path)
	if class == core.ErrorClassRemoteNotFound {
		err = core.ErrRemoteNotFound
	}
	return &core.ProviderError{
		Provider:   providerName,
		Operation:  strings.ToLower(req.Method),
		StatusCode: statusCode,
		Class:      class,
		Message:    strings.TrimSpace(string(body)),
		Err:        err,
	}
}

func classifyHTTPStatus(statusCode int, body string) core.ErrorClass {
	lowerBody := strings.ToLower(body)
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return core.ErrorClassAuth
	case statusCode == http.StatusNotFound:
		return core.ErrorClassRemoteNotFound
	case statusCode == http.StatusConflict:
		return core.ErrorClassConflict
	case statusCode == http.StatusTooManyRequests:
		return core.ErrorClassRateLimited
	case strings.Contains(lowerBody, "capacity") || strings.Contains(lowerBody, "available"):
		return core.ErrorClassCapacity
	case strings.Contains(lowerBody, "quota") || strings.Contains(lowerBody, "limit"):
		return core.ErrorClassQuota
	case statusCode >= 500:
		return core.ErrorClassTransient
	default:
		return core.ErrorClassUnknown
	}
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
