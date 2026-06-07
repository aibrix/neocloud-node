package lambda

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
	burstresource "github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/resource"
)

func (c *Client) Provider() string {
	return providerName
}

func (c *Client) ListOffers(ctx context.Context, intent burstresource.Intent) ([]burstresource.Offer, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/instance-types", nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(httpReq)

	var response instanceTypesResponse
	if err := c.doJSON(httpReq, http.StatusOK, &response); err != nil {
		return nil, err
	}
	entries, err := response.entries()
	if err != nil {
		return nil, err
	}

	offers := make([]burstresource.Offer, 0, len(entries))
	for _, entry := range entries {
		offers = append(offers, offersFromInstanceTypeEntry(entry, intent)...)
	}
	return offers, nil
}

type instanceTypesResponse struct {
	Data json.RawMessage `json:"data"`
}

func (r instanceTypesResponse) entries() ([]instanceTypeCatalogEntry, error) {
	if len(r.Data) == 0 || string(r.Data) == "null" {
		return nil, nil
	}

	var byName map[string]instanceTypeCatalogEntry
	if err := json.Unmarshal(r.Data, &byName); err == nil {
		entries := make([]instanceTypeCatalogEntry, 0, len(byName))
		for name, entry := range byName {
			if entry.Name == "" {
				entry.Name = name
			}
			entries = append(entries, entry)
		}
		return entries, nil
	}

	var list []instanceTypeCatalogEntry
	if err := json.Unmarshal(r.Data, &list); err != nil {
		return nil, err
	}
	return list, nil
}

type instanceTypeCatalogEntry struct {
	Name                         string                    `json:"name"`
	PriceCentsPerHour            int                       `json:"price_cents_per_hour"`
	Specs                        instanceTypeCatalogSpecs  `json:"specs"`
	InstanceType                 *instanceTypeCatalogEntry `json:"instance_type"`
	RegionsWithCapacityAvailable []catalogRegion           `json:"regions_with_capacity_available"`
}

type instanceTypeCatalogSpecs struct {
	GPUs           int    `json:"gpus"`
	GPUDescription string `json:"gpu_description"`
}

type catalogRegion struct {
	Name string `json:"name"`
}

func offersFromInstanceTypeEntry(entry instanceTypeCatalogEntry, intent burstresource.Intent) []burstresource.Offer {
	instanceType := entry
	if entry.InstanceType != nil {
		instanceType = *entry.InstanceType
		if instanceType.Name == "" {
			instanceType.Name = entry.Name
		}
	}

	name := strings.TrimSpace(instanceType.Name)
	gpuCount := instanceType.Specs.GPUs
	gpuType := firstNonEmpty(instanceType.Specs.GPUDescription, name)
	if name == "" || gpuCount <= 0 || strings.TrimSpace(gpuType) == "" {
		return nil
	}
	if intent.GPUCount <= 0 && gpuCount != 1 {
		return nil
	}
	if intent.GPUCount > 0 && gpuCount != intent.GPUCount {
		return nil
	}
	if strings.TrimSpace(intent.GPUType) != "" && intent.GPUType != gpuType && intent.GPUType != name {
		return nil
	}

	offers := make([]burstresource.Offer, 0, len(entry.RegionsWithCapacityAvailable))
	for _, region := range entry.RegionsWithCapacityAvailable {
		location := strings.TrimSpace(region.Name)
		if location == "" {
			continue
		}
		offers = append(offers, burstresource.Offer{
			ID:          providerName + ":" + name + ":" + location,
			Provider:    providerName,
			Location:    location,
			GPUType:     gpuType,
			GPUCount:    gpuCount,
			Market:      burstresource.MarketOnDemand,
			HourlyPrice: float64(instanceType.PriceCentsPerHour) / 100.0,
			Available:   true,
			Capabilities: map[burstresource.Capability]bool{
				burstresource.CapabilityCloudInit: true,
				burstresource.CapabilityPublicIP:  true,
			},
			ProviderHints: map[string]string{
				core.AnnotationLambdaInstanceType: name,
			},
		})
	}
	return offers
}
