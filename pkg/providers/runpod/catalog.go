package runpod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/aibrix/neocloud-node/pkg/core"
	burstresource "github.com/aibrix/neocloud-node/pkg/resource"
)

var _ burstresource.ProviderOfferSource = (*Client)(nil)

func (c *Client) Provider() string {
	return providerName
}

func (c *Client) ListOffers(ctx context.Context, intent burstresource.Intent) ([]burstresource.Offer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if intent.Market == burstresource.MarketSpot {
		return nil, nil
	}
	count := intent.GPUCount
	if count <= 0 {
		count = 1
	}

	var offers []burstresource.Offer
	for _, cloud := range []string{"SECURE", "COMMUNITY"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/catalog/gpus", nil)
		if err != nil {
			return nil, err
		}
		query := req.URL.Query()
		query.Set("include", "AVAILABILITY")
		query.Set("product", "POD")
		query.Set("cloud", cloud)
		query.Set("count", strconv.Itoa(count))
		req.URL.RawQuery = query.Encode()
		c.setHeaders(req)

		var response struct {
			GPUs []catalogGPU `json:"gpus"`
		}
		if err := c.doJSON(req, http.StatusOK, &response); err != nil {
			return nil, err
		}
		if response.GPUs == nil {
			return nil, providerDecodeError(req, errors.New("missing gpus in catalog response"))
		}
		offers = append(offers, offersFromCatalogGPUs(response.GPUs, cloud, count, intent)...)
	}
	return offers, nil
}

// Fields follow GpuType in https://api.runpod.io/v2/openapi.json.
type catalogGPU struct {
	ID           string              `json:"id"`
	Memory       int                 `json:"memory"`
	Secure       bool                `json:"secure"`
	Community    bool                `json:"community"`
	Price        map[string]*float64 `json:"price"`
	MaxCount     map[string]int      `json:"maxCount"`
	Availability string              `json:"availability"`
	DataCenters  []struct {
		ID           string `json:"id"`
		Availability string `json:"availability"`
	} `json:"dataCenters"`
}

func offersFromCatalogGPUs(gpus []catalogGPU, cloud string, count int, intent burstresource.Intent) []burstresource.Offer {
	var offers []burstresource.Offer
	tier := strings.ToLower(cloud)
	gpuType := strings.TrimSpace(intent.GPUType)
	for _, gpu := range gpus {
		if strings.TrimSpace(gpu.ID) == "" || (gpuType != "" && gpuType != gpu.ID) {
			continue
		}
		if (cloud == "SECURE" && !gpu.Secure) || (cloud == "COMMUNITY" && !gpu.Community) {
			continue
		}
		price := gpu.Price[tier]
		if price == nil || *price <= 0 || count > gpu.MaxCount[tier] {
			continue
		}
		for _, dc := range gpu.DataCenters {
			if strings.TrimSpace(dc.ID) == "" {
				continue
			}
			offer := burstresource.Offer{
				ID:          fmt.Sprintf("%s:%s:%s:%s:%d", providerName, gpu.ID, cloud, dc.ID, count),
				Provider:    providerName,
				Location:    dc.ID,
				GPUType:     gpu.ID,
				GPUCount:    count,
				Market:      burstresource.MarketOnDemand,
				HourlyPrice: *price * float64(count),
				Available:   catalogStockAvailable(gpu.Availability) && catalogStockAvailable(dc.Availability),
				Capabilities: map[burstresource.Capability]bool{
					burstresource.CapabilityPorts:         true,
					burstresource.CapabilityRegistryAuth:  true,
					burstresource.CapabilityNetworkVolume: cloud == "SECURE",
				},
				ProviderHints: map[string]string{
					core.AnnotationRunPodCloudType:     cloud,
					core.AnnotationRunPodDataCenterIDs: dc.ID,
				},
			}
			if gpu.Memory > 0 {
				offer.GPUMemory = fmt.Sprintf("%dGi", gpu.Memory)
			}
			offers = append(offers, offer)
		}
	}
	return offers
}

func catalogStockAvailable(stock string) bool {
	switch stock {
	case "LOW", "MEDIUM", "HIGH":
		return true
	default:
		return false
	}
}
