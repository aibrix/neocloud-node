package runpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
)

const providerName = "runpod"

const defaultBaseURL = "https://api.runpod.io/v2"

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
		baseURL = defaultBaseURL
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
	payload, err := newCreatePodRequest(name, request)
	if err != nil {
		return core.RemoteResource{}, err
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
			existing, findErr := c.FindByExternalName(ctx, name)
			if findErr == nil {
				existing.IdempotencyKey = idempotencyKey
				existing.Request = request
				return existing, nil
			}
			if !errors.Is(findErr, core.ErrRemoteNotFound) {
				return core.RemoteResource{}, findErr
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

// Stop stops a pod, which releases its GPU and keeps its disk.
func (c *Client) Stop(ctx context.Context, remoteID string) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/pods/"+remoteID+"/action", strings.NewReader(`{"action":"stop"}`))
	if err != nil {
		return err
	}
	c.setHeaders(httpReq)
	if err := c.doJSON(httpReq, http.StatusOK, nil); err != nil {
		if errors.Is(err, core.ErrRemoteNotFound) {
			return nil
		}
		return err
	}
	return nil
}

func (c *Client) List(ctx context.Context, filter PodFilter) ([]core.RemoteResource, error) {
	var resources []core.RemoteResource
	cursor := ""
	seen := make(map[string]bool)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/pods", nil)
		if err != nil {
			return nil, err
		}
		query := req.URL.Query()
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		req.URL.RawQuery = query.Encode()
		c.setHeaders(req)
		var page struct {
			Pods       []podResponse `json:"pods"`
			Pagination *struct {
				NextCursor  string `json:"nextCursor"`
				HasNextPage *bool  `json:"hasNextPage"`
			} `json:"pagination"`
		}
		if err := c.doJSON(req, http.StatusOK, &page); err != nil {
			return nil, err
		}
		if page.Pods == nil || page.Pagination == nil || page.Pagination.HasNextPage == nil {
			return nil, providerDecodeError(req, errors.New("missing pods or pagination in list response"))
		}
		for _, pod := range page.Pods {
			if filter.Name == "" || pod.Name == filter.Name {
				resources = append(resources, remoteFromPod(pod, "", core.WorkloadRequest{}))
			}
		}
		if !*page.Pagination.HasNextPage {
			return resources, nil
		}
		cursor = page.Pagination.NextCursor
		if cursor == "" || seen[cursor] {
			return nil, providerDecodeError(req, errors.New("missing or repeated pagination cursor"))
		}
		seen[cursor] = true
	}
}

func (c *Client) ListRemoteResources(ctx context.Context) ([]core.RemoteResource, error) {
	return c.List(ctx, PodFilter{})
}

func (c *Client) FindByExternalName(ctx context.Context, name string) (core.RemoteResource, error) {
	pods, err := c.List(ctx, PodFilter{Name: name})
	if err != nil {
		return core.RemoteResource{}, err
	}
	var found *core.RemoteResource
	for _, pod := range pods {
		if pod.ID == "" || pod.Phase == core.RemoteDeleted {
			continue
		}
		if found != nil {
			return core.RemoteResource{}, &core.ProviderError{
				Provider: providerName, Operation: "list", Class: core.ErrorClassConflict,
				Message: "multiple pods match the external name",
			}
		}
		match := pod
		found = &match
	}
	if found != nil {
		return *found, nil
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
		err = providerTransportError(httpReq, err)
		if c.deletedAfterUncertainDelete(ctx, remoteID, err) {
			return nil
		}
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if err != nil {
			return providerTransportError(httpReq, err)
		}
		err = providerHTTPError(httpReq, resp.StatusCode, body)
		if c.deletedAfterUncertainDelete(ctx, remoteID, err) {
			return nil
		}
		return err
	}
}

func (c *Client) deletedAfterUncertainDelete(ctx context.Context, remoteID string, deleteErr error) bool {
	if strings.TrimSpace(remoteID) == "" || !shouldRecoverDelete(deleteErr) {
		return false
	}
	_, err := c.Get(ctx, remoteID)
	return errors.Is(err, core.ErrRemoteNotFound) || core.ClassifyError(err) == core.ErrorClassRemoteNotFound
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

// podResponse contains the fields consumed from the v2 Pod schema.
type podResponse struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Cost   *float64 `json:"cost"`
	GPU    struct {
		ID    string `json:"id"`
		Count int    `json:"count"`
	} `json:"gpu"`
	DataCenterID string    `json:"dataCenterId"`
	Mounts       podMounts `json:"mounts"`
	Runtime      *struct {
		Uptime int64 `json:"uptime"`
		Ports  []struct {
			Private int    `json:"private"`
			Public  int    `json:"public"`
			Type    string `json:"type"`
			IP      string `json:"ip"`
		} `json:"ports"`
	} `json:"runtime"`
}

type PodFilter struct {
	Name string
}

func remoteFromPod(pod podResponse, idempotencyKey string, request core.WorkloadRequest) core.RemoteResource {
	request = requestWithPodShape(request, pod)
	reason, message := statusDetailFromStatus(pod.Status)
	return core.RemoteResource{
		ID:              pod.ID,
		IdempotencyKey:  idempotencyKey,
		ExternalName:    pod.Name,
		Provider:        providerName,
		Phase:           phaseFromStatus(pod.Status),
		Request:         request,
		Endpoints:       endpointsFromPod(pod),
		HourlyCost:      hourlyCost(pod),
		CostSource:      costSource(pod),
		ContainerUptime: containerUptime(pod),
		StatusReason:    reason,
		StatusMessage:   message,
	}
}

func requestWithPodShape(request core.WorkloadRequest, pod podResponse) core.WorkloadRequest {
	if request.Provider == "" {
		request.Provider = providerName
	}
	if request.GPUCount <= 0 {
		request.GPUCount = pod.GPU.Count
	}
	if strings.TrimSpace(request.GPUType) == "" {
		request.GPUType = strings.TrimSpace(pod.GPU.ID)
	}
	if strings.TrimSpace(request.RegionHint) == "" {
		request.RegionHint = strings.TrimSpace(pod.DataCenterID)
	}
	if strings.TrimSpace(request.MarketType) == "" {
		request.MarketType = core.MarketTypeOnDemand
	}

	if request.ProviderHints == nil {
		request.ProviderHints = map[string]string{}
	}
	if request.RegionHint != "" && request.ProviderHints[core.AnnotationRunPodDataCenterIDs] == "" {
		request.ProviderHints[core.AnnotationRunPodDataCenterIDs] = request.RegionHint
	}
	if len(pod.Mounts.Network) == 1 && request.ProviderHints[core.AnnotationRunPodNetworkVolumeID] == "" {
		request.ProviderHints[core.AnnotationRunPodNetworkVolumeID] = pod.Mounts.Network[0].VolumeID
		request.ProviderHints[core.AnnotationRunPodVolumeMountPath] = pod.Mounts.Network[0].Path
	}
	if len(request.ProviderHints) == 0 {
		request.ProviderHints = nil
	}
	return request
}

// ReasonRemoteExited marks a RunPod pod that stopped without a known exit code.
const ReasonRemoteExited = "RemoteExited"

// phaseFromStatus maps RunPod status to a remote phase. RunPod does not
// report the container exit code, so EXITED cannot be told apart from a crash
// or a reclaimed host and is reported as failed rather than succeeded.
func phaseFromStatus(status string) core.RemotePhase {
	switch status {
	case "RUNNING":
		return core.RemoteRunning
	case "EXITED", "ERROR":
		return core.RemoteFailed
	case "PROVISIONING", "STARTING":
		return core.RemotePending
	case "TERMINATED":
		return core.RemoteDeleted
	default:
		return core.RemotePending
	}
}

func statusDetailFromStatus(status string) (string, string) {
	if status == "EXITED" {
		return ReasonRemoteExited, "RunPod reported EXITED; the container exit code is not available"
	}
	if status == "ERROR" {
		return "RemoteError", "RunPod reported an unrecoverable container error"
	}
	return "", ""
}

func hourlyCost(pod podResponse) float64 {
	if pod.Cost != nil {
		return *pod.Cost
	}
	return 0
}

func costSource(pod podResponse) string {
	if pod.Cost != nil {
		return "runpod.cost"
	}
	return ""
}

func containerUptime(pod podResponse) time.Duration {
	if pod.Status != "RUNNING" || pod.Runtime == nil || pod.Runtime.Uptime <= 0 {
		return 0
	}
	return time.Duration(pod.Runtime.Uptime) * time.Second
}

func externalName(idempotencyKey string) string {
	name := core.ManagedExternalName(idempotencyKey)
	if len(name) > 191 {
		return name[:191]
	}
	return name
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

func shouldRecoverDelete(err error) bool {
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
	message := strings.TrimSpace(string(body))
	var problem struct {
		Title  string   `json:"title"`
		Detail string   `json:"detail"`
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(body, &problem) == nil && problem.Title != "" {
		parts := []string{problem.Title}
		if problem.Detail != "" {
			parts = append(parts, problem.Detail)
		}
		parts = append(parts, problem.Errors...)
		message = strings.Join(parts, ": ")
	}
	class := classifyHTTPStatus(statusCode, message)
	err := fmt.Errorf("unexpected status %d for %s %s", statusCode, req.Method, req.URL.Path)
	if class == core.ErrorClassRemoteNotFound {
		err = core.ErrRemoteNotFound
	}
	return &core.ProviderError{
		Provider:   providerName,
		Operation:  strings.ToLower(req.Method),
		StatusCode: statusCode,
		Class:      class,
		Message:    message,
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
	case statusCode == http.StatusPaymentRequired:
		return core.ErrorClassQuota
	case statusCode == http.StatusUnprocessableEntity || statusCode == http.StatusRequestEntityTooLarge:
		return core.ErrorClassUnsupportedSpec
	case statusCode >= 500:
		return core.ErrorClassTransient
	case strings.Contains(lowerBody, "capacity") || strings.Contains(lowerBody, "no longer any instances available"):
		return core.ErrorClassCapacity
	case strings.Contains(lowerBody, "quota") || strings.Contains(lowerBody, "limit"):
		return core.ErrorClassQuota
	case statusCode == http.StatusBadRequest:
		// RunPod rejected the request itself, for example a schema error.
		return core.ErrorClassUnsupportedSpec
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

// endpointsFromPod exposes only public TCP mappings reported by the runtime.
func endpointsFromPod(pod podResponse) []core.Endpoint {
	if pod.Runtime == nil {
		return nil
	}
	var endpoints []core.Endpoint
	for _, port := range pod.Runtime.Ports {
		host := strings.TrimSpace(port.IP)
		if port.Type != "tcp" || host == "" || port.Private <= 0 || port.Private > 65535 || port.Public <= 0 || port.Public > 65535 {
			continue
		}
		endpoints = append(endpoints, core.Endpoint{
			ContainerPort: port.Private, Protocol: "TCP", Host: host, Port: port.Public,
		})
	}
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].ContainerPort < endpoints[j].ContainerPort })
	return endpoints
}
