package vast

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
)

func TestLiveVastLifecycle(t *testing.T) {
	if os.Getenv("VAST_E2E") != "1" {
		t.Skip("set VAST_E2E=1 to run the paid Vast.ai lifecycle test")
	}
	apiKey := os.Getenv("VAST_API_KEY")
	if apiKey == "" {
		t.Fatal("VAST_API_KEY is required")
	}

	client := NewClient(Config{
		APIKey:             apiKey,
		BaseURL:            os.Getenv("VAST_BASE_URL"),
		V1BaseURL:          os.Getenv("VAST_V1_BASE_URL"),
		DiskGB:             liveFloatEnv(t, "VAST_E2E_DISK_GB", 20),
		MinimumReliability: liveFloatEnv(t, "VAST_E2E_MIN_RELIABILITY", 0.95),
		OfferLimit:         liveIntEnv(t, "VAST_E2E_OFFER_LIMIT", 100),
	})
	idempotencyKey := "e2e/vast-go-" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	externalName := externalName(idempotencyKey)
	request := core.WorkloadRequest{
		Image:      liveStringEnv("VAST_E2E_IMAGE", "python:3.12-slim"),
		Command:    []string{"/bin/sh", "-lc"},
		Args:       []string{"echo neocloud-vast-e2e-ready; exec sleep 600"},
		GPUCount:   1,
		GPUType:    liveStringEnv("VAST_E2E_GPU_TYPE", "GTX 1070"),
		MaxPrice:   liveStringEnv("VAST_E2E_MAX_PRICE", "0.10"),
		MarketType: core.MarketTypeOnDemand,
		Provider:   providerName,
		ProviderHints: map[string]string{
			core.AnnotationVastVerified: liveStringEnv("VAST_E2E_VERIFIED", "true"),
			core.AnnotationVastDiskGB:   liveStringEnv("VAST_E2E_DISK_GB", "20"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	startedAt := time.Now()
	remoteID := ""
	deleted := false
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cleanupCancel()
		if !deleted && remoteID != "" {
			if err := client.Delete(cleanupCtx, remoteID); err != nil {
				t.Errorf("cleanup Delete(%s): %v", remoteID, err)
			}
		}
		if existing, err := client.FindByExternalName(cleanupCtx, externalName); err == nil {
			if deleteErr := client.Delete(cleanupCtx, existing.ID); deleteErr != nil {
				t.Errorf("cleanup deterministic instance %s: %v", existing.ID, deleteErr)
			}
		} else if !isRemoteNotFound(err) {
			t.Errorf("cleanup lookup %s: %v", externalName, err)
		}
	})

	remote, err := client.Create(ctx, idempotencyKey, request)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	remoteID = remote.ID
	t.Logf("created Vast instance id=%s externalName=%s gpu=%s hourlyUSD=%.6f", remoteID, externalName, request.GPUType, remote.HourlyCost)

	duplicate, err := client.Create(ctx, idempotencyKey, request)
	if err != nil {
		t.Fatalf("idempotent Create() error = %v", err)
	}
	if duplicate.ID != remoteID {
		t.Fatalf("idempotent Create ID = %s, want %s", duplicate.ID, remoteID)
	}

	readyDeadline := time.Now().Add(liveDurationEnv(t, "VAST_E2E_READY_TIMEOUT", 6*time.Minute))
	for {
		observed, getErr := client.Get(ctx, remoteID)
		if getErr != nil {
			t.Fatalf("Get(%s) error = %v", remoteID, getErr)
		}
		t.Logf("instance id=%s phase=%s hourlyUSD=%.6f", remoteID, observed.Phase, observed.HourlyCost)
		if observed.Phase == core.RemoteRunning {
			break
		}
		if observed.Phase == core.RemoteFailed || observed.Phase == core.RemoteDeleted {
			t.Fatalf("instance entered terminal phase %s before readiness", observed.Phase)
		}
		if time.Now().After(readyDeadline) {
			t.Fatalf("timed out waiting for instance %s to run", remoteID)
		}
		time.Sleep(5 * time.Second)
	}

	remotes, err := client.ListRemoteResources(ctx)
	if err != nil {
		t.Fatalf("ListRemoteResources() error = %v", err)
	}
	found := false
	for _, item := range remotes {
		if item.ID == remoteID && item.ExternalName == externalName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("instance %s was not returned by ListRemoteResources", remoteID)
	}

	if err := client.Delete(ctx, remoteID); err != nil {
		t.Fatalf("Delete(%s) error = %v", remoteID, err)
	}
	deleteDeadline := time.Now().Add(liveDurationEnv(t, "VAST_E2E_DELETE_TIMEOUT", 2*time.Minute))
	for {
		_, getErr := client.Get(ctx, remoteID)
		if isRemoteNotFound(getErr) {
			deleted = true
			break
		}
		if getErr != nil {
			t.Fatalf("verify delete Get(%s) error = %v", remoteID, getErr)
		}
		if time.Now().After(deleteDeadline) {
			t.Fatalf("timed out waiting for instance %s deletion", remoteID)
		}
		time.Sleep(3 * time.Second)
	}
	if _, err := client.FindByExternalName(ctx, externalName); !errors.Is(err, core.ErrRemoteNotFound) {
		t.Fatalf("FindByExternalName after delete error = %v, want remote not found", err)
	}
	t.Logf("destroyed Vast instance id=%s duration=%s", remoteID, time.Since(startedAt).Round(time.Second))
}

func liveStringEnv(key string, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func liveFloatEnv(t *testing.T, key string, fallback float64) float64 {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("%s must be a number: %v", key, err)
	}
	return value
}

func liveIntEnv(t *testing.T, key string, fallback int) int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s must be an integer: %v", key, err)
	}
	return value
}

func liveDurationEnv(t *testing.T, key string, fallback time.Duration) time.Duration {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("%s must be a duration: %v", key, err)
	}
	return value
}
