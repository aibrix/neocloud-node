package vk

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type logTailPresence struct{}

// WithLogTailPresence preserves an explicit tailLines=0 across VK's int options.
func WithLogTailPresence(ctx context.Context, present bool) context.Context {
	return context.WithValue(ctx, logTailPresence{}, present)
}

func LogTailSpecified(ctx context.Context) bool {
	specified, _ := ctx.Value(logTailPresence{}).(bool)
	return specified
}

func (p *Provider) containerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o := core.LogOptions{SinceTime: opts.SinceTime, Follow: opts.Follow, Timestamps: opts.Timestamps, Previous: opts.Previous, LimitBytes: int64(opts.LimitBytes)}
	present := LogTailSpecified(ctx)
	if present || opts.Tail != 0 {
		tail := opts.Tail
		o.Tail = &tail
	}
	if opts.SinceSeconds > 0 {
		if !opts.SinceTime.IsZero() {
			return nil, errdefs.InvalidInput("sinceSeconds and sinceTime are mutually exclusive")
		}
		o.SinceTime = time.Now().Add(-time.Duration(opts.SinceSeconds) * time.Second)
	}
	if err := o.Validate(); err != nil {
		return nil, errdefs.AsInvalidInput(err)
	}
	pod := p.getCachedPod(podKey(namespace, podName))
	if p.podClient != nil {
		current, err := p.podClient.Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		pod = current
	}
	if pod == nil || pod.UID == "" || pod.Spec.NodeName != p.nodeName {
		return nil, errdefs.NotFound("pod does not belong to this virtual node")
	}
	if len(pod.Spec.Containers) != 1 {
		return nil, errdefs.InvalidInput("container logs require exactly one supported container")
	}
	if containerName == "" {
		containerName = pod.Spec.Containers[0].Name
	}
	if containerName != pod.Spec.Containers[0].Name {
		return nil, errdefs.InvalidInput("unknown container")
	}
	binding, err := p.store.GetByWorkloadUID(ctx, namespace, string(pod.UID))
	if errors.Is(err, core.ErrBindingNotFound) {
		return nil, errdefs.NotFound("pod binding not found")
	}
	if err != nil {
		return nil, err
	}
	if binding.Namespace != namespace || binding.WorkloadUID != string(pod.UID) || binding.RemoteID == "" || binding.Phase == core.BindingDeleted || binding.Deleting {
		return nil, errdefs.NotFound("pod has no readable remote resource")
	}
	if core.FindCondition(binding.Conditions, core.ConditionRemoteCleanedUp) != nil {
		return nil, errdefs.NotFound("remote resource cleanup has started; logs are no longer available")
	}
	selected := binding.Placement.Provider
	if selected == "" {
		selected = binding.Request.Provider
	}
	reader := p.logReaders[selected]
	if reader == nil {
		return nil, unsupportedMethod("container logs for selected provider")
	}
	out, err := reader.OpenLogs(ctx, core.LogTarget{Namespace: namespace, WorkloadUID: string(pod.UID), RemoteID: binding.RemoteID, ContainerName: containerName}, o)
	if errors.Is(err, core.ErrRemoteNotFound) {
		return nil, errdefs.NotFound("remote log resource not found")
	}
	if err != nil {
		return nil, err
	}
	return &logEventReader{ReadCloser: out, ctx: ctx, notify: func(err error) {
		p.recordPodEvent(pod, corev1.EventTypeWarning, "ContainerLogStreamFailed", err.Error())
	}}, nil
}

// API-server stream proxies may turn a late backend read failure into clean EOF.
// Preserve observability independently of the client's HTTP stream status.
type logEventReader struct {
	io.ReadCloser
	ctx      context.Context
	notify   func(error)
	reported sync.Once
	closed   atomic.Bool
}

func (r *logEventReader) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	if err != nil && !errors.Is(err, io.EOF) && r.ctx.Err() == nil && !r.closed.Load() {
		r.reported.Do(func() { r.notify(err) })
	}
	return n, err
}
func (r *logEventReader) Close() error { r.closed.Store(true); return r.ReadCloser.Close() }
