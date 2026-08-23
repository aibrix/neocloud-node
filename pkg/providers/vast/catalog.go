package vast

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/aibrix/neocloud-node/pkg/core"
	burstresource "github.com/aibrix/neocloud-node/pkg/resource"
)

func (c *Client) Provider() string {
	return providerName
}

func (c *Client) ListOffers(ctx context.Context, intent burstresource.Intent) ([]burstresource.Offer, error) {
	markets := []string{core.MarketTypeOnDemand, core.MarketTypeSpot}
	switch intent.Market {
	case burstresource.MarketOnDemand:
		markets = []string{core.MarketTypeOnDemand}
	case burstresource.MarketSpot:
		markets = []string{core.MarketTypeSpot}
	}

	request := core.WorkloadRequest{
		GPUCount:   intent.GPUCount,
		GPUType:    intent.GPUType,
		GPUMemory:  intent.GPUMemory,
		MarketType: core.MarketTypeOnDemand,
		Provider:   providerName,
	}
	if request.GPUCount <= 0 {
		request.GPUCount = 1
	}
	if intent.MaxHourlyPrice > 0 {
		request.MaxPrice = strconv.FormatFloat(intent.MaxHourlyPrice, 'f', -1, 64)
	}

	var all []burstresource.Offer
	for _, market := range markets {
		request.MarketType = market
		searchBody, normalizedMarket, _, err := c.createSearchBody(request, c.diskGB)
		if err != nil {
			return nil, err
		}
		offers, err := c.searchOffers(ctx, searchBody)
		if err != nil {
			return nil, err
		}
		for _, item := range offers {
			if item.identifier() == "" || strings.TrimSpace(item.GPUName) == "" || !item.NumGPUs.set || item.NumGPUs.value <= 0 {
				continue
			}
			providerHints := map[string]string{
				core.AnnotationVastOfferID: item.identifier(),
			}
			if machineID := item.MachineID.String(); machineID != "" {
				providerHints[core.AnnotationVastMachineID] = machineID
			}
			if item.Verified != nil {
				providerHints[core.AnnotationVastVerified] = strconv.FormatBool(*item.Verified)
			} else if verification := strings.TrimSpace(item.Verification); verification != "" {
				providerHints[core.AnnotationVastVerified] = strconv.FormatBool(strings.EqualFold(verification, "verified"))
			}
			providerHints[core.AnnotationVastDiskGB] = strconv.FormatFloat(c.diskGB, 'f', -1, 64)

			marketValue := burstresource.MarketOnDemand
			if normalizedMarket == core.MarketTypeSpot {
				marketValue = burstresource.MarketSpot
			}
			offer := burstresource.Offer{
				ID:            providerName + ":" + item.identifier() + ":" + normalizedMarket,
				Provider:      providerName,
				Location:      strings.TrimSpace(item.Geolocation),
				GPUType:       strings.TrimSpace(item.GPUName),
				GPUCount:      int(item.NumGPUs.value),
				Market:        marketValue,
				HourlyPrice:   offerPrice(item, normalizedMarket),
				Available:     true,
				ProviderHints: providerHints,
				Capabilities: map[burstresource.Capability]bool{
					burstresource.CapabilityPorts: true,
				},
			}
			if item.GPURAM.set && item.GPURAM.value > 0 {
				offer.GPUMemory = fmt.Sprintf("%gGi", item.GPURAM.value)
			}
			all = append(all, offer)
		}
	}
	return all, nil
}
