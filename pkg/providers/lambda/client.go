package lambda

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/aibrix/neocloud-node/pkg/runner"
)

const providerName = "lambda"

type Config struct {
	BaseURL             string
	APIKey              string
	CallbackURL         string
	CallbackSecret      string
	DefaultRegion       string
	DefaultInstanceType string
	DefaultSSHKeyNames  []string
	HTTPClient          *http.Client
}

type Client struct {
	baseURL             string
	apiKey              string
	callbackURL         string
	callbackSecret      string
	defaultRegion       string
	defaultInstanceType string
	defaultSSHKeyNames  []string
	httpClient          *http.Client
}

func NewClient(config Config) *Client {
	baseURL := strings.TrimRight(config.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://cloud.lambda.ai/api/v1"
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	return &Client{
		baseURL:             baseURL,
		apiKey:              config.APIKey,
		callbackURL:         config.CallbackURL,
		callbackSecret:      config.CallbackSecret,
		defaultRegion:       config.DefaultRegion,
		defaultInstanceType: config.DefaultInstanceType,
		defaultSSHKeyNames:  append([]string(nil), config.DefaultSSHKeyNames...),
		httpClient:          httpClient,
	}
}

func (c *Client) Create(ctx context.Context, idempotencyKey string, request core.WorkloadRequest) (core.RemoteResource, error) {
	name := externalName(idempotencyKey)
	payload, err := c.launchPayload(idempotencyKey, name, request)
	if err != nil {
		return core.RemoteResource{}, err
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return core.RemoteResource{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/instance-operations/launch", bytes.NewReader(body))
	if err != nil {
		return core.RemoteResource{}, err
	}
	c.setHeaders(httpReq)

	var response launchResponse
	if err := c.doJSON(httpReq, http.StatusOK, &response); err != nil {
		if shouldRecoverCreate(err) {
			if existing, findErr := c.FindByExternalName(ctx, name); findErr == nil {
				existing.IdempotencyKey = idempotencyKey
				existing.Request = request
				return existing, nil
			}
		}
		return core.RemoteResource{}, err
	}
	if len(response.Data.InstanceIDs) == 0 {
		return core.RemoteResource{}, &core.ProviderError{
			Provider:  providerName,
			Operation: "create",
			Class:     core.ErrorClassUnknown,
			Message:   "launch response did not include instance id",
		}
	}
	return core.RemoteResource{
		ID:             response.Data.InstanceIDs[0],
		IdempotencyKey: idempotencyKey,
		Provider:       providerName,
		Phase:          core.RemotePending,
		Request:        request,
	}, nil
}

func (c *Client) Get(ctx context.Context, remoteID string) (core.RemoteResource, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/instances/"+remoteID, nil)
	if err != nil {
		return core.RemoteResource{}, err
	}
	c.setHeaders(httpReq)

	var response instanceResponse
	if err := c.doJSON(httpReq, http.StatusOK, &response); err != nil {
		return core.RemoteResource{}, err
	}
	return remoteFromInstance(response.Data), nil
}

func (c *Client) List(ctx context.Context, filter InstanceFilter) ([]core.RemoteResource, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/instances", nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(httpReq)

	var response instancesResponse
	if err := c.doJSON(httpReq, http.StatusOK, &response); err != nil {
		return nil, err
	}

	resources := make([]core.RemoteResource, 0, len(response.Data))
	for _, instance := range response.Data {
		if filter.Name != "" && instance.Name != filter.Name {
			continue
		}
		resources = append(resources, remoteFromInstance(instance))
	}
	return resources, nil
}

func (c *Client) ListRemoteResources(ctx context.Context) ([]core.RemoteResource, error) {
	return c.List(ctx, InstanceFilter{})
}

func (c *Client) FindByExternalName(ctx context.Context, name string) (core.RemoteResource, error) {
	instances, err := c.List(ctx, InstanceFilter{Name: name})
	if err != nil {
		return core.RemoteResource{}, err
	}
	for _, instance := range instances {
		if instance.ID != "" {
			return instance, nil
		}
	}
	return core.RemoteResource{}, &core.ProviderError{
		Provider:  providerName,
		Operation: "list",
		Class:     core.ErrorClassRemoteNotFound,
		Message:   "instance not found by external name",
		Err:       core.ErrRemoteNotFound,
	}
}

func (c *Client) Delete(ctx context.Context, remoteID string) error {
	payload := terminateRequest{InstanceIDs: []string{remoteID}}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/instance-operations/terminate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.setHeaders(httpReq)

	var response terminateResponse
	if err := c.doJSON(httpReq, http.StatusOK, &response); err != nil {
		if strings.TrimSpace(remoteID) != "" && isRemoteNotFound(err) {
			return nil
		}
		if c.deletedAfterUncertainDelete(ctx, remoteID, err) {
			return nil
		}
		return err
	}
	return nil
}

func (c *Client) deletedAfterUncertainDelete(ctx context.Context, remoteID string, deleteErr error) bool {
	if strings.TrimSpace(remoteID) == "" || !shouldRecoverDelete(deleteErr) {
		return false
	}
	_, err := c.Get(ctx, remoteID)
	return isRemoteNotFound(err)
}

func (c *Client) launchPayload(idempotencyKey string, name string, request core.WorkloadRequest) (launchRequest, error) {
	region := firstNonEmpty(request.RegionHint, c.defaultRegion)
	instanceType := firstNonEmpty(providerHint(request, core.AnnotationLambdaInstanceType), c.defaultInstanceType)
	sshKeyNames := splitCSV(providerHint(request, core.AnnotationLambdaSSHKeyNames))
	if len(sshKeyNames) == 0 {
		sshKeyNames = append([]string(nil), c.defaultSSHKeyNames...)
	}

	if region == "" {
		return launchRequest{}, &core.ProviderError{Provider: providerName, Operation: "create", Class: core.ErrorClassUnsupportedSpec, Message: "lambda region is required"}
	}
	if instanceType == "" {
		return launchRequest{}, &core.ProviderError{Provider: providerName, Operation: "create", Class: core.ErrorClassUnsupportedSpec, Message: "lambda instance type is required"}
	}
	if len(sshKeyNames) == 0 {
		return launchRequest{}, &core.ProviderError{Provider: providerName, Operation: "create", Class: core.ErrorClassUnsupportedSpec, Message: "lambda ssh key name is required"}
	}

	payload := launchRequest{
		RegionName:       region,
		InstanceTypeName: instanceType,
		SSHKeyNames:      sshKeyNames,
		Name:             name,
		Hostname:         name,
		Tags: []tag{
			{Key: "gpu-burst-node:aibrix:workload", Value: idempotencyKey},
			{Key: "gpu-burst-node:aibrix:managed-by", Value: "gpu-burst-node"},
		},
	}
	if imageID := providerHint(request, core.AnnotationLambdaImageID); imageID != "" {
		payload.Image = &image{ID: imageID}
	}
	for _, filesystemID := range splitCSV(providerHint(request, core.AnnotationLambdaFileSystemIDs)) {
		payload.FileSystemMounts = append(payload.FileSystemMounts, fileSystemMount{
			FileSystemID: filesystemID,
			MountPoint:   "/lambda/nfs/" + sanitizeMountName(filesystemID),
		})
	}
	for _, firewallID := range splitCSV(providerHint(request, core.AnnotationLambdaFirewallIDs)) {
		payload.FirewallRulesets = append(payload.FirewallRulesets, resourceRef{ID: firewallID})
	}
	if c.callbackURL != "" {
		cloudInit, err := runner.BuildCloudInit(runner.CloudInitConfig{
			WorkloadID:     idempotencyKey,
			CallbackURL:    c.callbackURL,
			CallbackSecret: c.callbackSecret,
			Request:        request,
		})
		if err != nil {
			return launchRequest{}, err
		}
		payload.UserData = cloudInit
	}
	return payload, nil
}

func (c *Client) setHeaders(req *http.Request) {
	if c.apiKey != "" {
		req.SetBasicAuth(c.apiKey, "")
	}
	req.Header.Set("Accept", "application/json")
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

type launchRequest struct {
	RegionName       string            `json:"region_name"`
	InstanceTypeName string            `json:"instance_type_name"`
	SSHKeyNames      []string          `json:"ssh_key_names"`
	FileSystemMounts []fileSystemMount `json:"file_system_mounts,omitempty"`
	Hostname         string            `json:"hostname,omitempty"`
	Name             string            `json:"name,omitempty"`
	Image            *image            `json:"image,omitempty"`
	UserData         string            `json:"user_data,omitempty"`
	Tags             []tag             `json:"tags,omitempty"`
	FirewallRulesets []resourceRef     `json:"firewall_rulesets,omitempty"`
}

type fileSystemMount struct {
	MountPoint   string `json:"mount_point"`
	FileSystemID string `json:"file_system_id"`
}

type image struct {
	ID string `json:"id"`
}

type tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type resourceRef struct {
	ID string `json:"id"`
}

type launchResponse struct {
	Data struct {
		InstanceIDs []string `json:"instance_ids"`
	} `json:"data"`
}

type terminateRequest struct {
	InstanceIDs []string `json:"instance_ids"`
}

type terminateResponse struct {
	Data struct {
		TerminatedInstances []instance `json:"terminated_instances"`
	} `json:"data"`
}

type instanceResponse struct {
	Data instance `json:"data"`
}

type instancesResponse struct {
	Data []instance `json:"data"`
}

type instance struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IP        string `json:"ip"`
	PrivateIP string `json:"private_ip"`
	Status    string `json:"status"`
	Region    struct {
		Name string `json:"name"`
	} `json:"region"`
	InstanceType struct {
		Name              string `json:"name"`
		GPUDescription    string `json:"gpu_description"`
		PriceCentsPerHour int    `json:"price_cents_per_hour"`
		Specs             struct {
			GPUs int `json:"gpus"`
		} `json:"specs"`
	} `json:"instance_type"`
}

type InstanceFilter struct {
	Name string
}

func remoteFromInstance(instance instance) core.RemoteResource {
	return core.RemoteResource{
		ID:           instance.ID,
		ExternalName: instance.Name,
		Provider:     providerName,
		Phase:        phaseFromStatus(instance.Status),
		Request:      requestFromInstance(instance),
		HourlyCost:   float64(instance.InstanceType.PriceCentsPerHour) / 100.0,
		CostSource:   costSource(instance),
	}
}

func requestFromInstance(instance instance) core.WorkloadRequest {
	request := core.WorkloadRequest{
		Provider:   providerName,
		RegionHint: strings.TrimSpace(instance.Region.Name),
		GPUType:    firstNonEmpty(instance.InstanceType.GPUDescription, instance.InstanceType.Name),
		GPUCount:   instance.InstanceType.Specs.GPUs,
		MarketType: core.MarketTypeOnDemand,
	}
	if instance.InstanceType.Name != "" {
		request.ProviderHints = map[string]string{
			core.AnnotationLambdaInstanceType: instance.InstanceType.Name,
		}
	}
	return request
}

func costSource(instance instance) string {
	if instance.InstanceType.PriceCentsPerHour <= 0 {
		return ""
	}
	return "lambda.instance_type.price_cents_per_hour"
}

func phaseFromStatus(status string) core.RemotePhase {
	switch strings.ToLower(status) {
	case "active":
		return core.RemoteRunning
	case "terminated":
		return core.RemoteDeleted
	case "unhealthy", "preempted":
		return core.RemoteFailed
	default:
		return core.RemotePending
	}
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func shouldRecoverCreate(err error) bool {
	class := core.ClassifyError(err)
	return class == core.ErrorClassTransient || class == core.ErrorClassTimeout
}

func shouldRecoverDelete(err error) bool {
	class := core.ClassifyError(err)
	return class == core.ErrorClassTransient || class == core.ErrorClassTimeout
}

func isRemoteNotFound(err error) bool {
	return core.ClassifyError(err) == core.ErrorClassRemoteNotFound
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
	case statusCode >= 500:
		return core.ErrorClassTransient
	case strings.Contains(lowerBody, "insufficient-capacity") || strings.Contains(lowerBody, "capacity"):
		return core.ErrorClassCapacity
	case strings.Contains(lowerBody, "quota") || strings.Contains(lowerBody, "limit"):
		return core.ErrorClassQuota
	case strings.Contains(lowerBody, "invalid-api-key"):
		return core.ErrorClassAuth
	default:
		return core.ErrorClassUnknown
	}
}

func externalName(idempotencyKey string) string {
	name := core.ManagedExternalName(idempotencyKey)
	if len(name) > 64 {
		return name[:64]
	}
	return name
}

func sanitizeMountName(value string) string {
	value = strings.ToLower(value)
	replacer := strings.NewReplacer("/", "-", "_", "-", ".", "-", ":", "-")
	return strings.Trim(replacer.Replace(value), "-")
}
