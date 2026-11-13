package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aibrix/neocloud-node/pkg/core"
)

func TestCreateRejectsUnsupportedSpecsBeforeHTTP(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*core.WorkloadRequest)
		field  string
	}{
		{"spot", func(r *core.WorkloadRequest) { r.MarketType = core.MarketTypeSpot }, core.AnnotationMarketType},
		{"data center priority", func(r *core.WorkloadRequest) { r.ProviderHints[core.AnnotationRunPodDataCenterPriority] = "custom" }, core.AnnotationRunPodDataCenterPriority},
		{"missing GPU", func(r *core.WorkloadRequest) { r.GPUType = "" }, core.AnnotationGPUType},
		{"GPU fallback list", func(r *core.WorkloadRequest) { r.GPUType = "NVIDIA A40,NVIDIA A100" }, core.AnnotationGPUType},
		{"zero GPUs", func(r *core.WorkloadRequest) { r.GPUCount = 0 }, "gpuCount"},
		{"template path without volume", func(r *core.WorkloadRequest) {
			r.ProviderHints[core.AnnotationRunPodTemplateID] = "template-test"
			r.ProviderHints[core.AnnotationRunPodVolumeMountPath] = "/data"
		}, core.AnnotationRunPodVolumeMountPath},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := core.WorkloadRequest{GPUType: "NVIDIA A40", GPUCount: 1, ProviderHints: map[string]string{}}
			tt.change(&r)
			client := NewClient(Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("unsupported request reached HTTP")
				return nil, errors.New("unexpected HTTP request")
			})}})
			_, err := client.Create(context.Background(), "default/test", r)
			var unsupported *core.UnsupportedSpecError
			if !errors.As(err, &unsupported) || unsupported.Field != tt.field {
				t.Fatalf("error = %v, want UnsupportedSpecError for %s", err, tt.field)
			}
		})
	}
}

func TestCreateStorageMapping(t *testing.T) {
	for _, tt := range []struct {
		name  string
		hints map[string]string
		want  string
	}{
		{"v1 default preserved", nil, `{"persistent":{"size":20,"path":"/workspace"}}`},
		{"custom persistent path", map[string]string{core.AnnotationRunPodVolumeMountPath: "/data"}, `{"persistent":{"size":20,"path":"/data"}}`},
		{"network default path", map[string]string{core.AnnotationRunPodNetworkVolumeID: "volume-test"}, `{"network":[{"volumeId":"volume-test","path":"/workspace"}]}`},
		{"template storage inherited", map[string]string{core.AnnotationRunPodTemplateID: "template-test"}, ``},
	} {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := newCreatePodRequest("test", core.WorkloadRequest{GPUType: "NVIDIA A40", GPUCount: 1, ProviderHints: tt.hints})
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["mounts"]) != tt.want {
				t.Fatalf("mounts=%s, want %s", fields["mounts"], tt.want)
			}
			wantDisk := "50"
			if tt.hints[core.AnnotationRunPodTemplateID] != "" {
				wantDisk = ""
			}
			if string(fields["disk"]) != wantDisk {
				t.Fatalf("disk=%s, want %s", fields["disk"], wantDisk)
			}
		})
	}
}

func TestFindByExternalNamePaginatesAndFilters(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/pods" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		switch requests {
		case 1:
			if r.URL.RawQuery != "" {
				t.Errorf("unexpected query %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"pods":[{"id":"unrelated","name":"other","status":"RUNNING"}],"pagination":{"hasNextPage":true,"nextCursor":"cursor+/="}}`))
		case 2:
			if len(r.URL.Query()) != 1 || r.URL.Query().Get("cursor") != "cursor+/=" {
				t.Errorf("unexpected query %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"pods":[{"id":"expected","name":"target","status":"STARTING"}],"pagination":{"hasNextPage":false,"nextCursor":null}}`))
		default:
			t.Errorf("unexpected request %d", requests)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	remote, err := NewClient(Config{BaseURL: server.URL}).FindByExternalName(context.Background(), "target")
	if err != nil || remote.ID != "expected" || requests != 2 {
		t.Fatalf("remote=%#v err=%v requests=%d", remote, err, requests)
	}
}

func TestListRejectsIncompletePagination(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
	}{
		{"missing wrapper", `[]`, 200},
		{"missing pods", `{"pagination":{"hasNextPage":false}}`, 200},
		{"missing metadata", `{"pods":[]}`, 200},
		{"missing continuation flag", `{"pods":[],"pagination":{}}`, 200},
		{"missing cursor", `{"pods":[],"pagination":{"hasNextPage":true}}`, 200},
		{"repeated cursor", `{"pods":[],"pagination":{"hasNextPage":true,"nextCursor":"again"}}`, 200},
		{"page failure", `{"title":"Bad Gateway","status":502,"detail":"upstream failure"}`, 502},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls > 2 {
					t.Error("pagination did not stop")
					w.WriteHeader(500)
					return
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			remotes, err := NewClient(Config{BaseURL: server.URL}).ListRemoteResources(context.Background())
			if err == nil || remotes != nil || core.ClassifyError(err) != core.ErrorClassTransient {
				t.Fatalf("list=%#v, err=%v", remotes, err)
			}
		})
	}
}

func TestFindByExternalNameDoesNotAdoptAmbiguousOrDeletedPods(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		class      core.ErrorClass
	}{
		{"not found", `{"pods":[{"id":"other","name":"other","status":"RUNNING"}],"pagination":{"hasNextPage":false,"nextCursor":null}}`, core.ErrorClassRemoteNotFound},
		{"terminated", `{"pods":[{"id":"gone","name":"target","status":"TERMINATED"}],"pagination":{"hasNextPage":false,"nextCursor":null}}`, core.ErrorClassRemoteNotFound},
		{"ambiguous", `{"pods":[{"id":"one","name":"target","status":"RUNNING"},{"id":"two","name":"target","status":"STARTING"}],"pagination":{"hasNextPage":false,"nextCursor":null}}`, core.ErrorClassConflict},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tt.body)) }))
			defer server.Close()
			_, err := NewClient(Config{BaseURL: server.URL}).FindByExternalName(context.Background(), "target")
			if core.ClassifyError(err) != tt.class {
				t.Fatalf("error=%v, want %s", err, tt.class)
			}
		})
	}
}

func TestCreateRecoversFromLaterListPage(t *testing.T) {
	creates, lists := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			creates++
			http.Error(w, "lost create response", http.StatusGatewayTimeout)
			return
		}
		lists++
		if lists == 1 {
			_, _ = w.Write([]byte(`{"pods":[{"id":"other","name":"other","status":"RUNNING"}],"pagination":{"hasNextPage":true,"nextCursor":"next"}}`))
			return
		}
		if lists != 2 || r.URL.Query().Get("cursor") != "next" {
			t.Errorf("unexpected recovery request %s", r.URL)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"pods":[{"id":"recovered","name":"neocloud-node-default-test","status":"PROVISIONING"}],"pagination":{"hasNextPage":false,"nextCursor":null}}`))
	}))
	defer server.Close()
	request := core.WorkloadRequest{Image: "busybox:1.36", GPUType: "NVIDIA A40", GPUCount: 1}
	remote, err := NewClient(Config{BaseURL: server.URL}).Create(context.Background(), "default/test", request)
	if err != nil || remote.ID != "recovered" || remote.IdempotencyKey != "default/test" || remote.Request.Image != request.Image {
		t.Fatalf("remote=%#v error=%v", remote, err)
	}
	if creates != 1 || lists != 2 {
		t.Fatalf("creates=%d lists=%d, want one create and two list pages", creates, lists)
	}
}

func TestCreateSurfacesRecoveryConflict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Error(w, "lost create response", http.StatusGatewayTimeout)
			return
		}
		_, _ = w.Write([]byte(`{"pods":[{"id":"one","name":"neocloud-node-default-test","status":"RUNNING"},{"id":"two","name":"neocloud-node-default-test","status":"STARTING"}],"pagination":{"hasNextPage":false,"nextCursor":null}}`))
	}))
	defer server.Close()
	_, err := NewClient(Config{BaseURL: server.URL}).Create(context.Background(), "default/test", core.WorkloadRequest{
		Image: "busybox:1.36", GPUType: "NVIDIA A40", GPUCount: 1,
	})
	if core.ClassifyError(err) != core.ErrorClassConflict {
		t.Fatalf("error=%v, want recovery conflict", err)
	}
}
