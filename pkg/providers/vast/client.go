package vast

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
)

const (
	providerName              = "vast"
	defaultBaseURL            = "https://console.vast.ai/api/v0"
	defaultDiskGB             = 20.0
	defaultMinimumReliability = 0.95
	defaultOfferLimit         = 100
	maximumExternalNameLength = 64
)

type Config struct {
	BaseURL            string
	V1BaseURL          string
	APIKey             string
	HTTPClient         *http.Client
	DiskGB             float64
	MinimumReliability float64
	OfferLimit         int
}

type Client struct {
	baseURL            string
	v1BaseURL          string
	apiKey             string
	httpClient         *http.Client
	diskGB             float64
	minimumReliability float64
	offerLimit         int
}

func NewClient(config Config) *Client {
	baseURL := strings.TrimRight(config.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	v1BaseURL := strings.TrimRight(config.V1BaseURL, "/")
	if v1BaseURL == "" {
		v1BaseURL = versionedBaseURL(baseURL, "v1")
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	diskGB := config.DiskGB
	if diskGB <= 0 {
		diskGB = defaultDiskGB
	}
	minimumReliability := config.MinimumReliability
	if minimumReliability <= 0 {
		minimumReliability = defaultMinimumReliability
	}
	offerLimit := config.OfferLimit
	if offerLimit <= 0 {
		offerLimit = defaultOfferLimit
	}

	return &Client{
		baseURL:            baseURL,
		v1BaseURL:          v1BaseURL,
		apiKey:             config.APIKey,
		httpClient:         httpClient,
		diskGB:             diskGB,
		minimumReliability: minimumReliability,
		offerLimit:         offerLimit,
	}
}

func (c *Client) Create(ctx context.Context, idempotencyKey string, request core.WorkloadRequest) (core.RemoteResource, error) {
	name := externalName(idempotencyKey)
	if existing, err := c.FindByExternalName(ctx, name); err == nil {
		existing.IdempotencyKey = idempotencyKey
		existing.Request = request
		return existing, nil
	} else if !isRemoteNotFound(err) {
		return core.RemoteResource{}, err
	}

	if strings.TrimSpace(request.Image) == "" {
		return core.RemoteResource{}, unsupportedSpecError("create", "Vast requires a container image")
	}
	if request.GPUCount <= 0 {
		return core.RemoteResource{}, unsupportedSpecError("create", "Vast requires a positive GPU count")
	}

	diskGB, err := c.diskGBForRequest(request)
	if err != nil {
		return core.RemoteResource{}, err
	}
	searchBody, market, maxPrice, err := c.createSearchBody(request, diskGB)
	if err != nil {
		return core.RemoteResource{}, err
	}
	offers, err := c.searchOffers(ctx, searchBody)
	if err != nil {
		return core.RemoteResource{}, err
	}
	if len(offers) == 0 {
		return core.RemoteResource{}, capacityError("search", "no Vast offers matched the workload request")
	}
	offer := offers[0]
	price := offerPrice(offer, market)
	if maxPrice > 0 && (price <= 0 || price > maxPrice) {
		return core.RemoteResource{}, capacityError("search", fmt.Sprintf("cheapest Vast offer costs %.6f USD/hour, above max price %.6f", price, maxPrice))
	}

	payload := createInstanceRequest{
		ClientID:      "me",
		Image:         request.Image,
		Env:           vastEnvironment(request),
		Disk:          diskGB,
		Label:         name,
		RunType:       "args",
		CancelUnavail: true,
	}
	if len(request.Command) > 0 {
		payload.OnStart = request.Command[0]
		payload.Args = append(append([]string(nil), request.Command[1:]...), request.Args...)
	} else {
		payload.Args = append([]string(nil), request.Args...)
	}
	if market == core.MarketTypeSpot {
		if !offer.MinBid.set || offer.MinBid.value <= 0 {
			return core.RemoteResource{}, unsupportedSpecError("create", "Vast interruptible offer did not include a positive min_bid")
		}
		payload.Price = offer.MinBid.value
	}

	var response createInstanceResponse
	path := "/asks/" + url.PathEscape(offer.identifier()) + "/"
	if err := c.requestJSON(ctx, http.MethodPut, c.baseURL+path, "create", payload, http.StatusOK, &response); err != nil {
		if shouldRecoverMutation(err) {
			if existing, findErr := c.FindByExternalName(ctx, name); findErr == nil {
				existing.IdempotencyKey = idempotencyKey
				existing.Request = request
				return existing, nil
			}
		}
		return core.RemoteResource{}, err
	}
	if response.Success != nil && !*response.Success {
		err := providerSemanticError("create", response.rawMessage())
		if shouldRecoverMutation(err) {
			if existing, findErr := c.FindByExternalName(ctx, name); findErr == nil {
				existing.IdempotencyKey = idempotencyKey
				existing.Request = request
				return existing, nil
			}
		}
		return core.RemoteResource{}, err
	}
	remoteID := response.NewContract.String()
	if remoteID == "" {
		if existing, findErr := c.FindByExternalName(ctx, name); findErr == nil {
			existing.IdempotencyKey = idempotencyKey
			existing.Request = request
			return existing, nil
		}
		return core.RemoteResource{}, &core.ProviderError{
			Provider:  providerName,
			Operation: "create",
			Class:     core.ErrorClassTransient,
			Message:   "Vast create response did not include new_contract",
		}
	}

	return core.RemoteResource{
		ID:             remoteID,
		IdempotencyKey: idempotencyKey,
		ExternalName:   name,
		Provider:       providerName,
		Phase:          core.RemotePending,
		Request:        request,
		HourlyCost:     price,
		CostSource:     offerCostSource(offer, market),
	}, nil
}

func (c *Client) Get(ctx context.Context, remoteID string) (core.RemoteResource, error) {
	var response instanceResponse
	path := "/instances/" + url.PathEscape(remoteID) + "/"
	if err := c.requestJSON(ctx, http.MethodGet, c.baseURL+path, "get", nil, http.StatusOK, &response); err != nil {
		return core.RemoteResource{}, err
	}
	instance, ok, err := response.instance()
	if err != nil {
		return core.RemoteResource{}, providerDecodeError("get", err)
	}
	if !ok || instance.isDeleted() {
		return core.RemoteResource{}, remoteNotFoundError("get", "Vast instance not found")
	}
	return remoteFromInstance(instance, "", core.WorkloadRequest{}), nil
}

func (c *Client) ListRemoteResources(ctx context.Context) ([]core.RemoteResource, error) {
	instances, err := c.listInstances(ctx, "")
	if err != nil {
		return nil, err
	}
	resources := make([]core.RemoteResource, 0, len(instances))
	for _, instance := range instances {
		if instance.isDeleted() {
			continue
		}
		resources = append(resources, remoteFromInstance(instance, "", core.WorkloadRequest{}))
	}
	return resources, nil
}

func (c *Client) FindByExternalName(ctx context.Context, name string) (core.RemoteResource, error) {
	instances, err := c.listInstances(ctx, name)
	if err != nil {
		return core.RemoteResource{}, err
	}
	matches := make([]instance, 0, 1)
	for _, item := range instances {
		if item.Label == name && !item.isDeleted() {
			matches = append(matches, item)
		}
	}
	switch len(matches) {
	case 0:
		return core.RemoteResource{}, remoteNotFoundError("list", "Vast instance not found by external name")
	case 1:
		return remoteFromInstance(matches[0], "", core.WorkloadRequest{}), nil
	default:
		return core.RemoteResource{}, &core.ProviderError{
			Provider:  providerName,
			Operation: "list",
			Class:     core.ErrorClassConflict,
			Message:   "multiple Vast instances matched the deterministic external name",
		}
	}
}

func (c *Client) Delete(ctx context.Context, remoteID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/instances/"+url.PathEscape(remoteID)+"/", nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		err = providerTransportError("delete", err)
		if c.deletedAfterUncertainDelete(ctx, remoteID, err) {
			return nil
		}
		return err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if readErr != nil {
		err = providerTransportError("delete", readErr)
		if c.deletedAfterUncertainDelete(ctx, remoteID, err) {
			return nil
		}
		return err
	}
	if resp.StatusCode == http.StatusNotFound || isMissingPayload(body) {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err = providerHTTPError("delete", resp.StatusCode, body)
		if c.deletedAfterUncertainDelete(ctx, remoteID, err) {
			return nil
		}
		return err
	}
	if semanticFailure(body) {
		err = providerSemanticError("delete", body)
		if c.deletedAfterUncertainDelete(ctx, remoteID, err) {
			return nil
		}
		return err
	}
	return nil
}

func (c *Client) deletedAfterUncertainDelete(ctx context.Context, remoteID string, deleteErr error) bool {
	if strings.TrimSpace(remoteID) == "" || !shouldRecoverMutation(deleteErr) {
		return false
	}
	_, err := c.Get(ctx, remoteID)
	return isRemoteNotFound(err)
}

func (c *Client) createSearchBody(request core.WorkloadRequest, diskGB float64) (map[string]any, string, float64, error) {
	market, err := vastMarket(request.MarketType)
	if err != nil {
		return nil, "", 0, unsupportedSpecError("search", err.Error())
	}
	maxPrice, err := parseMaxPrice(request.MaxPrice)
	if err != nil {
		return nil, "", 0, unsupportedSpecError("search", err.Error())
	}
	verified, includeVerified, err := verifiedFilter(providerHint(request, core.AnnotationVastVerified))
	if err != nil {
		return nil, "", 0, unsupportedSpecError("search", err.Error())
	}

	body := map[string]any{
		"limit":             c.offerLimit,
		"type":              vastMarketType(market),
		"external":          map[string]any{"eq": false},
		"rentable":          map[string]any{"eq": true},
		"rented":            map[string]any{"eq": false},
		"num_gpus":          map[string]any{"eq": request.GPUCount},
		"reliability":       map[string]any{"gte": c.minimumReliability},
		"disk_space":        map[string]any{"gte": diskGB},
		"direct_port_count": map[string]any{"gte": len(request.Ports)},
		"order":             [][]any{{"dph_total", "asc"}, {"reliability", "desc"}},
		"allocated_storage": diskGB,
	}
	if includeVerified {
		body["verified"] = map[string]any{"eq": verified}
	}
	if gpuType := strings.TrimSpace(request.GPUType); gpuType != "" {
		gpuNames := normalizedGPUNames(gpuType)
		if len(gpuNames) == 1 {
			body["gpu_name"] = map[string]any{"eq": gpuNames[0]}
		} else {
			body["gpu_name"] = map[string]any{"in": gpuNames}
		}
	}
	if gpuMemory := strings.TrimSpace(request.GPUMemory); gpuMemory != "" {
		memoryGB, parseErr := parseStorageGB(gpuMemory)
		if parseErr != nil {
			return nil, "", 0, unsupportedSpecError("search", "invalid GPU memory: "+parseErr.Error())
		}
		body["gpu_ram"] = map[string]any{"gte": memoryGB}
	}
	if region := strings.TrimSpace(request.RegionHint); region != "" {
		body["geolocation"] = map[string]any{"eq": region}
	}
	if machineID := providerHint(request, core.AnnotationVastMachineID); machineID != "" {
		value, parseErr := strconv.ParseInt(machineID, 10, 64)
		if parseErr != nil || value <= 0 {
			return nil, "", 0, unsupportedSpecError("search", "Vast machine ID must be a positive integer")
		}
		body["machine_id"] = map[string]any{"eq": value}
	}
	if offerID := providerHint(request, core.AnnotationVastOfferID); offerID != "" {
		value, parseErr := strconv.ParseInt(offerID, 10, 64)
		if parseErr != nil || value <= 0 {
			return nil, "", 0, unsupportedSpecError("search", "Vast offer ID must be a positive integer")
		}
		body["id"] = map[string]any{"eq": value}
	}
	if maxPrice > 0 {
		field := "dph_total"
		if market == core.MarketTypeSpot {
			field = "min_bid"
		}
		body[field] = map[string]any{"lte": maxPrice}
	}
	return body, market, maxPrice, nil
}

func (c *Client) searchOffers(ctx context.Context, body map[string]any) ([]offer, error) {
	var response offersResponse
	if err := c.requestJSON(ctx, http.MethodPost, c.baseURL+"/bundles/", "search", body, http.StatusOK, &response); err != nil {
		return nil, err
	}
	if response.Success != nil && !*response.Success {
		return nil, providerSemanticError("search", response.rawMessage())
	}
	unique := make(map[string]offer, len(response.Offers))
	for _, item := range response.Offers {
		if id := item.identifier(); id != "" {
			unique[id] = item
		}
	}
	offers := make([]offer, 0, len(unique))
	for _, item := range unique {
		offers = append(offers, item)
	}
	sort.Slice(offers, func(i, j int) bool {
		left := offerPrice(offers[i], marketFromVastType(stringValue(body["type"])))
		right := offerPrice(offers[j], marketFromVastType(stringValue(body["type"])))
		if left != right {
			if left <= 0 {
				return false
			}
			if right <= 0 {
				return true
			}
			return left < right
		}
		if offers[i].Reliability.value != offers[j].Reliability.value {
			return offers[i].Reliability.value > offers[j].Reliability.value
		}
		return offers[i].identifier() < offers[j].identifier()
	})
	return offers, nil
}

func (c *Client) listInstances(ctx context.Context, name string) ([]instance, error) {
	selectFilters := map[string]any{}
	if name != "" {
		selectFilters["label"] = map[string]any{"in": []string{name}}
	}
	filterJSON, err := json.Marshal(selectFilters)
	if err != nil {
		return nil, err
	}
	orderJSON, err := json.Marshal([]map[string]string{{"col": "id", "dir": "asc"}})
	if err != nil {
		return nil, err
	}

	var instances []instance
	seenTokens := map[string]struct{}{}
	afterToken := ""
	for {
		query := url.Values{}
		query.Set("select_filters", string(filterJSON))
		query.Set("order_by", string(orderJSON))
		query.Set("limit", "25")
		if afterToken != "" {
			query.Set("after_token", afterToken)
		}
		var response instancesV1Response
		if err := c.requestJSON(ctx, http.MethodGet, c.v1BaseURL+"/instances/?"+query.Encode(), "list", nil, http.StatusOK, &response); err != nil {
			return nil, err
		}
		instances = append(instances, response.Instances...)
		nextToken := strings.TrimSpace(response.NextToken.String())
		if nextToken == "" {
			break
		}
		if _, seen := seenTokens[nextToken]; seen {
			break
		}
		seenTokens[nextToken] = struct{}{}
		afterToken = nextToken
	}
	return instances, nil
}

func (c *Client) diskGBForRequest(request core.WorkloadRequest) (float64, error) {
	raw := providerHint(request, core.AnnotationVastDiskGB)
	if raw == "" {
		return c.diskGB, nil
	}
	value, err := parseStorageGB(raw)
	if err != nil || value <= 0 {
		return 0, unsupportedSpecError("create", "Vast disk size must be a positive number of GiB")
	}
	return value, nil
}

func (c *Client) requestJSON(ctx context.Context, method string, requestURL string, operation string, payload any, expectedStatus int, out any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return providerTransportError(operation, err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return providerTransportError(operation, err)
	}
	if resp.StatusCode != expectedStatus {
		return providerHTTPError(operation, resp.StatusCode, responseBody)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(responseBody, out); err != nil {
		return providerDecodeError(operation, err)
	}
	return nil
}

func (c *Client) setHeaders(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Content-Type", "application/json")
}

type createInstanceRequest struct {
	ClientID      string            `json:"client_id"`
	Image         string            `json:"image"`
	Env           map[string]string `json:"env,omitempty"`
	Price         float64           `json:"price,omitempty"`
	Disk          float64           `json:"disk"`
	Label         string            `json:"label"`
	OnStart       string            `json:"onstart,omitempty"`
	RunType       string            `json:"runtype"`
	Args          []string          `json:"args,omitempty"`
	CancelUnavail bool              `json:"cancel_unavail"`
}

type createInstanceResponse struct {
	Success     *bool      `json:"success"`
	NewContract identifier `json:"new_contract"`
	Error       any        `json:"error"`
	Message     any        `json:"message"`
	Msg         any        `json:"msg"`
}

func (r createInstanceResponse) rawMessage() []byte {
	data, _ := json.Marshal(r)
	return data
}

type offersResponse struct {
	Success *bool   `json:"success"`
	Offers  []offer `json:"offers"`
	Error   any     `json:"error"`
	Message any     `json:"message"`
	Msg     any     `json:"msg"`
}

func (r offersResponse) rawMessage() []byte {
	data, _ := json.Marshal(r)
	return data
}

type offer struct {
	ID            identifier  `json:"id"`
	AskContractID identifier  `json:"ask_contract_id"`
	GPUName       string      `json:"gpu_name"`
	NumGPUs       numberValue `json:"num_gpus"`
	GPURAM        numberValue `json:"gpu_ram"`
	DPHTotal      numberValue `json:"dph_total"`
	DPHTotalAdj   numberValue `json:"dph_total_adj"`
	MinBid        numberValue `json:"min_bid"`
	Reliability   numberValue `json:"reliability"`
	Geolocation   string      `json:"geolocation"`
	MachineID     identifier  `json:"machine_id"`
	Verified      *bool       `json:"verified"`
	Verification  string      `json:"verification"`
	DiskSpace     numberValue `json:"disk_space"`
}

func (o offer) identifier() string {
	if value := o.ID.String(); value != "" {
		return value
	}
	return o.AskContractID.String()
}

type instanceResponse struct {
	Instances json.RawMessage `json:"instances"`
}

func (r instanceResponse) instance() (instance, bool, error) {
	trimmed := bytes.TrimSpace(r.Instances)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return instance{}, false, nil
	}
	if trimmed[0] == '[' {
		var items []instance
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return instance{}, false, err
		}
		if len(items) == 0 {
			return instance{}, false, nil
		}
		return items[0], true, nil
	}
	var item instance
	if err := json.Unmarshal(trimmed, &item); err != nil {
		return instance{}, false, err
	}
	return item, item.ID.String() != "", nil
}

type instancesV1Response struct {
	Instances []instance `json:"instances"`
	NextToken identifier `json:"next_token"`
}

type instance struct {
	ID             identifier  `json:"id"`
	AskContractID  identifier  `json:"ask_contract_id"`
	Label          string      `json:"label"`
	ActualStatus   string      `json:"actual_status"`
	CurState       string      `json:"cur_state"`
	IntendedStatus string      `json:"intended_status"`
	GPUName        string      `json:"gpu_name"`
	NumGPUs        numberValue `json:"num_gpus"`
	GPURAM         numberValue `json:"gpu_ram"`
	DPHTotal       numberValue `json:"dph_total"`
	DPHTotalAdj    numberValue `json:"dph_total_adj"`
	BidPrice       numberValue `json:"bid_price"`
	MinBid         numberValue `json:"min_bid"`
	IsBid          bool        `json:"is_bid"`
	Geolocation    string      `json:"geolocation"`
	MachineID      identifier  `json:"machine_id"`
	Verification   string      `json:"verification"`
	Image          string      `json:"image"`
	ImageUUID      string      `json:"image_uuid"`
	DiskSpace      numberValue `json:"disk_space"`
}

func (i instance) isDeleted() bool {
	status := strings.ToLower(strings.TrimSpace(i.ActualStatus))
	if status == "destroyed" || status == "terminated" || status == "deleted" {
		return true
	}
	return status == "" && strings.EqualFold(strings.TrimSpace(i.CurState), "unloaded")
}

type identifier struct {
	value string
}

func (i *identifier) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		return json.Unmarshal(data, &i.value)
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	i.value = number.String()
	return nil
}

func (i identifier) String() string {
	return strings.TrimSpace(i.value)
}

type numberValue struct {
	value float64
	set   bool
}

func (n *numberValue) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var raw string
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		if strings.TrimSpace(raw) == "" {
			return nil
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return err
		}
		n.value = value
		n.set = true
		return nil
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return err
	}
	n.value = value
	n.set = true
	return nil
}

func remoteFromInstance(instance instance, idempotencyKey string, request core.WorkloadRequest) core.RemoteResource {
	request = requestWithInstanceShape(request, instance)
	return core.RemoteResource{
		ID:             instance.ID.String(),
		IdempotencyKey: idempotencyKey,
		ExternalName:   instance.Label,
		Provider:       providerName,
		Phase:          phaseFromInstance(instance),
		Request:        request,
		HourlyCost:     instanceHourlyCost(instance),
		CostSource:     instanceCostSource(instance),
	}
}

func requestWithInstanceShape(request core.WorkloadRequest, instance instance) core.WorkloadRequest {
	if request.Provider == "" {
		request.Provider = providerName
	}
	if strings.TrimSpace(request.Image) == "" {
		request.Image = firstNonEmpty(instance.Image, instance.ImageUUID)
	}
	if request.GPUCount <= 0 && instance.NumGPUs.set {
		request.GPUCount = int(instance.NumGPUs.value)
	}
	if strings.TrimSpace(request.GPUType) == "" {
		request.GPUType = strings.TrimSpace(instance.GPUName)
	}
	if strings.TrimSpace(request.GPUMemory) == "" && instance.GPURAM.set {
		request.GPUMemory = strconv.FormatFloat(instance.GPURAM.value, 'f', -1, 64) + "Gi"
	}
	if strings.TrimSpace(request.RegionHint) == "" {
		request.RegionHint = strings.TrimSpace(instance.Geolocation)
	}
	if strings.TrimSpace(request.MarketType) == "" {
		if instance.IsBid || (instance.BidPrice.set && instance.BidPrice.value > 0) {
			request.MarketType = core.MarketTypeSpot
		} else {
			request.MarketType = core.MarketTypeOnDemand
		}
	}
	if request.ProviderHints == nil {
		request.ProviderHints = map[string]string{}
	}
	if machineID := instance.MachineID.String(); machineID != "" {
		request.ProviderHints[core.AnnotationVastMachineID] = machineID
	}
	if offerID := instance.AskContractID.String(); offerID != "" {
		request.ProviderHints[core.AnnotationVastOfferID] = offerID
	}
	if verification := strings.TrimSpace(instance.Verification); verification != "" {
		request.ProviderHints[core.AnnotationVastVerified] = strconv.FormatBool(strings.EqualFold(verification, "verified"))
	}
	if instance.DiskSpace.set && instance.DiskSpace.value > 0 {
		request.ProviderHints[core.AnnotationVastDiskGB] = strconv.FormatFloat(instance.DiskSpace.value, 'f', -1, 64)
	}
	if len(request.ProviderHints) == 0 {
		request.ProviderHints = nil
	}
	return request
}

func phaseFromInstance(instance instance) core.RemotePhase {
	switch strings.ToLower(strings.TrimSpace(instance.ActualStatus)) {
	case "running", "frozen":
		return core.RemoteRunning
	case "destroyed", "terminated", "deleted":
		return core.RemoteDeleted
	case "exited", "failed", "error", "unknown", "offline", "stopped":
		return core.RemoteFailed
	default:
		return core.RemotePending
	}
}

func instanceHourlyCost(instance instance) float64 {
	if instance.DPHTotal.set {
		return instance.DPHTotal.value
	}
	if instance.DPHTotalAdj.set {
		return instance.DPHTotalAdj.value
	}
	if instance.BidPrice.set {
		return instance.BidPrice.value
	}
	return 0
}

func instanceCostSource(instance instance) string {
	if instance.DPHTotal.set {
		return "vast.dph_total"
	}
	if instance.DPHTotalAdj.set {
		return "vast.dph_total_adj"
	}
	if instance.BidPrice.set {
		return "vast.bid_price"
	}
	return ""
}

func offerPrice(offer offer, market string) float64 {
	if market == core.MarketTypeSpot && offer.MinBid.set {
		return offer.MinBid.value
	}
	if offer.DPHTotal.set {
		return offer.DPHTotal.value
	}
	if offer.DPHTotalAdj.set {
		return offer.DPHTotalAdj.value
	}
	if offer.MinBid.set {
		return offer.MinBid.value
	}
	return 0
}

func offerCostSource(offer offer, market string) string {
	if market == core.MarketTypeSpot && offer.MinBid.set {
		return "vast.min_bid"
	}
	if offer.DPHTotal.set {
		return "vast.dph_total"
	}
	if offer.DPHTotalAdj.set {
		return "vast.dph_total_adj"
	}
	if offer.MinBid.set {
		return "vast.min_bid"
	}
	return ""
}

func vastEnvironment(request core.WorkloadRequest) map[string]string {
	if len(request.Env) == 0 && len(request.Ports) == 0 {
		return nil
	}
	environment := make(map[string]string, len(request.Env)+len(request.Ports))
	for key, value := range request.Env {
		environment[key] = value
	}
	for _, port := range request.Ports {
		protocol := strings.ToLower(strings.TrimSpace(port.Protocol))
		if protocol == "" {
			protocol = "tcp"
		}
		mapping := fmt.Sprintf("-p %d:%d", port.ContainerPort, port.ContainerPort)
		if protocol != "tcp" {
			mapping += "/" + protocol
		}
		environment[mapping] = "1"
	}
	return environment
}

func vastMarket(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", core.MarketTypeOnDemand, "ondemand", "on_demand":
		return core.MarketTypeOnDemand, nil
	case core.MarketTypeSpot, "interruptible", "preemptible", "bid":
		return core.MarketTypeSpot, nil
	default:
		return "", fmt.Errorf("Vast market must be on-demand or spot")
	}
}

func vastMarketType(market string) string {
	if market == core.MarketTypeSpot {
		return "bid"
	}
	return "on-demand"
}

func marketFromVastType(market string) string {
	if market == "bid" {
		return core.MarketTypeSpot
	}
	return core.MarketTypeOnDemand
}

func verifiedFilter(raw string) (bool, bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "true", "verified", "secure":
		return true, true, nil
	case "false", "unverified", "community":
		return false, true, nil
	case "any", "marketplace":
		return false, false, nil
	default:
		return false, false, fmt.Errorf("Vast verified hint must be true, false, or any")
	}
}

func parseMaxPrice(raw string) (float64, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("Vast max price must be a positive number")
	}
	return value, nil
}

func parseStorageGB(raw string) (float64, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	multiplier := 1.0
	for _, suffix := range []string{"gib", "gi", "gb", "g"} {
		if strings.HasSuffix(value, suffix) {
			value = strings.TrimSpace(strings.TrimSuffix(value, suffix))
			break
		}
	}
	for _, suffix := range []string{"mib", "mi", "mb", "m"} {
		if strings.HasSuffix(strings.ToLower(strings.TrimSpace(raw)), suffix) {
			value = strings.TrimSpace(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), suffix))
			multiplier = 1.0 / 1024.0
			break
		}
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("value must be positive")
	}
	return parsed * multiplier, nil
}

func normalizedGPUNames(raw string) []string {
	values := []string{strings.TrimSpace(raw), strings.ReplaceAll(strings.TrimSpace(raw), "_", " ")}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func externalName(idempotencyKey string) string {
	name := core.ManagedExternalName(idempotencyKey)
	if len(name) > maximumExternalNameLength {
		return name[:maximumExternalNameLength]
	}
	return name
}

func providerHint(request core.WorkloadRequest, key string) string {
	if len(request.ProviderHints) == 0 {
		return ""
	}
	return strings.TrimSpace(request.ProviderHints[key])
}

func versionedBaseURL(baseURL string, version string) string {
	index := strings.LastIndex(baseURL, "/api/v")
	if index >= 0 {
		return strings.TrimRight(baseURL[:index], "/") + "/api/" + version
	}
	return strings.TrimRight(baseURL, "/")
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func semanticFailure(body []byte) bool {
	var response struct {
		Success *bool `json:"success"`
	}
	return json.Unmarshal(body, &response) == nil && response.Success != nil && !*response.Success
}

func isMissingPayload(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "not_found") ||
		strings.Contains(lower, "not found") ||
		strings.Contains(lower, "no such instance") ||
		strings.Contains(lower, "no_such_instance")
}

func isRemoteNotFound(err error) bool {
	return errors.Is(err, core.ErrRemoteNotFound) || core.ClassifyError(err) == core.ErrorClassRemoteNotFound
}

func shouldRecoverMutation(err error) bool {
	class := core.ClassifyError(err)
	return class == core.ErrorClassTransient || class == core.ErrorClassTimeout
}

func providerTransportError(operation string, err error) error {
	class := core.ClassifyError(err)
	if class == core.ErrorClassUnknown {
		class = core.ErrorClassTransient
	}
	return &core.ProviderError{
		Provider:  providerName,
		Operation: operation,
		Class:     class,
		Message:   err.Error(),
		Err:       err,
	}
}

func providerDecodeError(operation string, err error) error {
	return &core.ProviderError{
		Provider:  providerName,
		Operation: operation,
		Class:     core.ErrorClassTransient,
		Message:   "decode provider response: " + err.Error(),
		Err:       err,
	}
}

func providerHTTPError(operation string, statusCode int, body []byte) error {
	class := classifyHTTPStatus(statusCode, string(body))
	underlying := fmt.Errorf("unexpected Vast status %d", statusCode)
	if class == core.ErrorClassRemoteNotFound {
		underlying = core.ErrRemoteNotFound
	}
	return &core.ProviderError{
		Provider:   providerName,
		Operation:  operation,
		StatusCode: statusCode,
		Class:      class,
		Message:    strings.TrimSpace(string(body)),
		Err:        underlying,
	}
}

func providerSemanticError(operation string, body []byte) error {
	class := classifyHTTPStatus(http.StatusOK, string(body))
	if class == core.ErrorClassRemoteNotFound {
		return remoteNotFoundError(operation, strings.TrimSpace(string(body)))
	}
	return &core.ProviderError{
		Provider:  providerName,
		Operation: operation,
		Class:     class,
		Message:   strings.TrimSpace(string(body)),
	}
}

func classifyHTTPStatus(statusCode int, body string) core.ErrorClass {
	lowerBody := strings.ToLower(body)
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return core.ErrorClassAuth
	case statusCode == http.StatusNotFound:
		return core.ErrorClassRemoteNotFound
	case statusCode == http.StatusGone || strings.Contains(lowerBody, "no_such_ask") || strings.Contains(lowerBody, "no such ask"):
		return core.ErrorClassCapacity
	case strings.Contains(lowerBody, "not available") || strings.Contains(lowerBody, "already rented") || strings.Contains(lowerBody, "capacity"):
		return core.ErrorClassCapacity
	case strings.Contains(lowerBody, "insufficient credit") || strings.Contains(lowerBody, "insufficient balance") || strings.Contains(lowerBody, "quota"):
		return core.ErrorClassQuota
	case statusCode == http.StatusBadRequest && (strings.Contains(lowerBody, "invalid_args") || strings.Contains(lowerBody, "invalid args") || strings.Contains(lowerBody, "invalid_request")):
		return core.ErrorClassUnsupportedSpec
	case statusCode == http.StatusConflict:
		return core.ErrorClassConflict
	case statusCode == http.StatusTooManyRequests:
		return core.ErrorClassRateLimited
	case statusCode >= 500:
		return core.ErrorClassTransient
	default:
		return core.ErrorClassUnknown
	}
}

func unsupportedSpecError(operation string, message string) error {
	return &core.ProviderError{
		Provider:  providerName,
		Operation: operation,
		Class:     core.ErrorClassUnsupportedSpec,
		Message:   message,
	}
}

func capacityError(operation string, message string) error {
	return &core.ProviderError{
		Provider:  providerName,
		Operation: operation,
		Class:     core.ErrorClassCapacity,
		Message:   message,
	}
}

func remoteNotFoundError(operation string, message string) error {
	return &core.ProviderError{
		Provider:  providerName,
		Operation: operation,
		Class:     core.ErrorClassRemoteNotFound,
		Message:   message,
		Err:       core.ErrRemoteNotFound,
	}
}
