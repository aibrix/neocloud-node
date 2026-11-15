package vk

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/aibrix/neocloud-node/pkg/logstream"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

type recordingLogReader struct {
	target core.LogTarget
	opts   core.LogOptions
	calls  int
}

type failingLogReader struct{}

func (failingLogReader) OpenLogs(ctx context.Context, target core.LogTarget, o core.LogOptions) (io.ReadCloser, error) {
	return logstream.Open(ctx, o, func(ctx context.Context, emit func(logstream.Event) error) error {
		if err := emit(logstream.Event{Line: "partial"}); err != nil {
			return err
		}
		return errors.New("upstream failed")
	}, logstream.Timing{})
}

func TestLogsStreamFailureEmitsPodWarning(t *testing.T) {
	ctx := context.Background()
	store := core.NewMemoryBindingStore()
	pod := gpuPod("logs", "uid-logs")
	pod.Spec.NodeName = "test-node"
	store.CreateOrGet(ctx, core.Binding{Namespace: "default", WorkloadUID: "uid-logs", RemoteID: "remote", Placement: core.Placement{Provider: "runpod"}})
	recorder := record.NewFakeRecorder(4)
	p, err := NewProvider(Config{Store: store, Adapter: core.NewFakeProvider("runpod"), NodeName: "test-node", PodClient: kubefake.NewSimpleClientset(pod).CoreV1(), LogReaders: map[string]core.ProviderLogReader{"runpod": failingLogReader{}}, EventRecorder: recorder})
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.GetContainerLogs(ctx, "default", "logs", "", api.ContainerLogOpts{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, err = io.ReadAll(r)
	if err == nil {
		t.Fatal("expected stream failure")
	}
	select {
	case e := <-recorder.Events:
		if !strings.Contains(e, "Warning ContainerLogStreamFailed") {
			t.Fatalf("event=%s", e)
		}
	default:
		t.Fatal("stream failure was not observable as a Pod event")
	}
}

func (r *recordingLogReader) OpenLogs(ctx context.Context, target core.LogTarget, o core.LogOptions) (io.ReadCloser, error) {
	r.target = target
	r.opts = o
	r.calls++
	return io.NopCloser(strings.NewReader("container output\n")), nil
}

func TestLogsResolveSelectedProviderWithoutLifecycleCalls(t *testing.T) {
	ctx := context.Background()
	store := core.NewMemoryBindingStore()
	pod := gpuPod("logs", "uid-logs")
	pod.Spec.NodeName = "test-node"
	_, _, err := store.CreateOrGet(ctx, core.Binding{Namespace: "default", WorkloadUID: "uid-logs", RemoteID: "lambda-remote", Placement: core.Placement{Provider: "lambda"}})
	if err != nil {
		t.Fatal(err)
	}
	reader := &recordingLogReader{}
	adapter := core.NewFakeProvider("runpod")
	p, err := NewProvider(Config{Store: store, Adapter: adapter, NodeName: "test-node", PodClient: kubefake.NewSimpleClientset(pod).CoreV1(), LogReaders: map[string]core.ProviderLogReader{"lambda": reader}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.GetContainerLogs(WithLogTailPresence(ctx, true), "default", "logs", pod.Spec.Containers[0].Name, api.ContainerLogOpts{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if string(b) != "container output\n" || reader.target.RemoteID != "lambda-remote" || reader.target.WorkloadUID != "uid-logs" || reader.opts.Tail == nil || *reader.opts.Tail != 0 {
		t.Fatalf("logs=%q target=%+v options=%+v", b, reader.target, reader.opts)
	}
	if adapter.CreateCount() != 0 {
		t.Fatal("log request allocated workload")
	}
}

func TestLogsRejectStaleUIDWrongNodeAndContainer(t *testing.T) {
	for _, variant := range []string{"uid", "node", "container", "unsupported", "pending"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			store := core.NewMemoryBindingStore()
			pod := gpuPod("logs", "uid-current")
			pod.Spec.NodeName = "test-node"
			uid := "uid-current"
			remote := "remote"
			name := pod.Spec.Containers[0].Name
			switch variant {
			case "uid":
				uid = "uid-old"
			case "node":
				pod.Spec.NodeName = "elsewhere"
			case "container":
				name = "missing"
			case "pending":
				remote = ""
			}
			store.CreateOrGet(ctx, core.Binding{Namespace: "default", WorkloadUID: uid, RemoteID: remote, Placement: core.Placement{Provider: "runpod"}})
			reader := &recordingLogReader{}
			readers := map[string]core.ProviderLogReader{"runpod": reader}
			if variant == "unsupported" {
				readers = nil
			}
			client := kubefake.NewSimpleClientset(pod)
			p, _ := NewProvider(Config{Store: store, Adapter: core.NewFakeProvider("runpod"), NodeName: "test-node", PodClient: client.CoreV1(), LogReaders: readers})
			// A stale local cache must never override the current API object's UID.
			old := pod.DeepCopy()
			old.UID = "uid-old"
			p.pods[podKey("default", "logs")] = old
			if r, err := p.GetContainerLogs(ctx, "default", "logs", name, api.ContainerLogOpts{}); err == nil {
				r.Close()
				t.Fatal("invalid request accepted")
			}
			if reader.calls != 0 {
				t.Fatal("invalid request reached provider")
			}
		})
	}
}
