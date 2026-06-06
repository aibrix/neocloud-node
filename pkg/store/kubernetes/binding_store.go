package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

const (
	CloudPodBindingGroup      = "gpu-burst-node.aibrix.ai"
	CloudPodBindingVersion    = "v1alpha1"
	CloudPodBindingKind       = "CloudPodBinding"
	CloudPodBindingResource   = "cloudpodbindings"
	CloudPodBindingAPIVersion = CloudPodBindingGroup + "/" + CloudPodBindingVersion

	CloudPodBindingFinalizer = "gpu-burst-node.aibrix.ai/finalizer"
	LabelPodUID              = "gpu-burst-node.aibrix.ai/pod-uid"
	LabelPodName             = "gpu-burst-node.aibrix.ai/pod-name"
)

var CloudPodBindingsGVR = schema.GroupVersionResource{
	Group:    CloudPodBindingGroup,
	Version:  CloudPodBindingVersion,
	Resource: CloudPodBindingResource,
}

type BindingStore struct {
	client dynamic.Interface
}

func NewBindingStore(client dynamic.Interface) *BindingStore {
	return &BindingStore{client: client}
}

func (s *BindingStore) CreateOrGet(ctx context.Context, binding core.Binding) (core.Binding, bool, error) {
	if err := ctx.Err(); err != nil {
		return core.Binding{}, false, err
	}

	existing, err := s.getObject(ctx, binding.Namespace, binding.WorkloadUID)
	if err == nil {
		parsed, err := bindingFromObject(existing)
		return parsed, false, err
	}
	if !apierrors.IsNotFound(err) {
		return core.Binding{}, false, err
	}

	if binding.Phase == "" {
		binding.Phase = core.BindingPending
	}
	created, err := s.resource(binding.Namespace).Create(ctx, objectForBinding(binding), metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := s.getObject(ctx, binding.Namespace, binding.WorkloadUID)
		if getErr != nil {
			return core.Binding{}, false, getErr
		}
		parsed, parseErr := bindingFromObject(existing)
		return parsed, false, parseErr
	}
	if err != nil {
		return core.Binding{}, false, err
	}

	updated, err := s.persistStatus(ctx, created, binding)
	if err != nil {
		return core.Binding{}, false, err
	}
	parsed, err := bindingFromObject(updated)
	return parsed, true, err
}

func (s *BindingStore) GetByWorkloadUID(ctx context.Context, namespace string, workloadUID string) (core.Binding, error) {
	if err := ctx.Err(); err != nil {
		return core.Binding{}, err
	}

	object, err := s.getObject(ctx, namespace, workloadUID)
	if apierrors.IsNotFound(err) {
		return core.Binding{}, core.ErrBindingNotFound
	}
	if err != nil {
		return core.Binding{}, err
	}
	return bindingFromObject(object)
}

func (s *BindingStore) Update(ctx context.Context, binding core.Binding) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	object, err := s.getObject(ctx, binding.Namespace, binding.WorkloadUID)
	if apierrors.IsNotFound(err) {
		return core.ErrBindingNotFound
	}
	if err != nil {
		return err
	}
	_, err = s.persistStatus(ctx, object, binding)
	return err
}

func (s *BindingStore) MarkDeleting(ctx context.Context, namespace string, workloadUID string) (core.Binding, error) {
	if err := ctx.Err(); err != nil {
		return core.Binding{}, err
	}

	object, err := s.getObject(ctx, namespace, workloadUID)
	if apierrors.IsNotFound(err) {
		return core.Binding{}, core.ErrBindingNotFound
	}
	if err != nil {
		return core.Binding{}, err
	}

	binding, err := bindingFromObject(object)
	if err != nil {
		return core.Binding{}, err
	}
	binding.Deleting = true
	binding.Phase = core.BindingDeleting

	updated, err := s.persistStatus(ctx, object, binding)
	if err != nil {
		return core.Binding{}, err
	}
	return bindingFromObject(updated)
}

func (s *BindingStore) CompleteDelete(ctx context.Context, namespace string, workloadUID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	object, err := s.getObject(ctx, namespace, workloadUID)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	object = object.DeepCopy()
	object.SetFinalizers(removeString(object.GetFinalizers(), CloudPodBindingFinalizer))
	if _, err := s.resource(namespace).Update(ctx, object, metav1.UpdateOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := s.resource(namespace).Delete(ctx, BindingName(workloadUID), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (s *BindingStore) getObject(ctx context.Context, namespace string, workloadUID string) (*unstructured.Unstructured, error) {
	return s.resource(namespace).Get(ctx, BindingName(workloadUID), metav1.GetOptions{})
}

func (s *BindingStore) persistStatus(ctx context.Context, object *unstructured.Unstructured, binding core.Binding) (*unstructured.Unstructured, error) {
	updated := object.DeepCopy()
	updated.Object["status"] = statusFromBinding(binding)

	out, err := s.resource(binding.Namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	if err == nil {
		return out, nil
	}
	return s.resource(binding.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
}

func (s *BindingStore) resource(namespace string) dynamic.ResourceInterface {
	return s.client.Resource(CloudPodBindingsGVR).Namespace(namespace)
}

func objectForBinding(binding core.Binding) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": CloudPodBindingAPIVersion,
			"kind":       CloudPodBindingKind,
			"metadata": map[string]interface{}{
				"name":      BindingName(binding.WorkloadUID),
				"namespace": binding.Namespace,
				"labels": map[string]interface{}{
					LabelPodUID:  binding.WorkloadUID,
					LabelPodName: binding.Name,
				},
				"finalizers": []interface{}{CloudPodBindingFinalizer},
			},
			"spec": map[string]interface{}{
				"podName":     binding.Name,
				"workloadUID": binding.WorkloadUID,
			},
			"status": statusFromBinding(binding),
		},
	}
}

func statusFromBinding(binding core.Binding) map[string]interface{} {
	return map[string]interface{}{
		"podName":     binding.Name,
		"workloadUID": binding.WorkloadUID,
		"phase":       string(binding.Phase),
		"remoteID":    binding.RemoteID,
		"request":     requestToMap(binding.Request),
		"placement":   placementToMap(binding.Placement),
		"attempts":    attemptsToSlice(binding.Attempts),
		"deleting":    binding.Deleting,
		"lastError":   binding.LastError,
	}
}

func bindingFromObject(object *unstructured.Unstructured) (core.Binding, error) {
	spec := asMap(object.Object["spec"])
	status := asMap(object.Object["status"])

	namespace := object.GetNamespace()
	name := stringValue(status["podName"])
	if name == "" {
		name = stringValue(spec["podName"])
	}
	workloadUID := stringValue(status["workloadUID"])
	if workloadUID == "" {
		workloadUID = stringValue(spec["workloadUID"])
	}

	return core.Binding{
		Namespace:   namespace,
		Name:        name,
		WorkloadUID: workloadUID,
		Request:     requestFromMap(asMap(status["request"])),
		RemoteID:    stringValue(status["remoteID"]),
		Phase:       core.BindingPhase(stringValue(status["phase"])),
		Placement:   placementFromMap(asMap(status["placement"])),
		Attempts:    attemptsFromSlice(asSlice(status["attempts"])),
		Deleting:    boolValue(status["deleting"]),
		LastError:   stringValue(status["lastError"]),
	}, nil
}

func BindingName(workloadUID string) string {
	normalized := strings.Trim(sanitizeName(workloadUID), "-.")
	if normalized == "" {
		normalized = hashString(workloadUID)
	}
	name := "pod-" + normalized
	if len(name) > 63 {
		name = "pod-" + hashString(workloadUID)
	}
	return strings.Trim(name, "-.")
}

func sanitizeName(value string) string {
	value = strings.ToLower(value)
	var builder strings.Builder
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) && r <= unicode.MaxASCII:
			builder.WriteRune(r)
		case unicode.IsDigit(r):
			builder.WriteRune(r)
		case r == '-' || r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	return builder.String()
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:32]
}

func requestToMap(request core.WorkloadRequest) map[string]interface{} {
	return map[string]interface{}{
		"image":            request.Image,
		"command":          stringSliceToInterface(request.Command),
		"args":             stringSliceToInterface(request.Args),
		"env":              stringMapToInterface(request.Env),
		"gpuCount":         int64(request.GPUCount),
		"gpuType":          request.GPUType,
		"maxPrice":         request.MaxPrice,
		"provider":         request.Provider,
		"regionHint":       request.RegionHint,
		"restartPolicy":    request.RestartPolicy,
		"imagePullSecrets": stringSliceToInterface(request.ImagePullSecrets),
		"ports":            portsToSlice(request.Ports),
	}
}

func requestFromMap(values map[string]interface{}) core.WorkloadRequest {
	return core.WorkloadRequest{
		Image:            stringValue(values["image"]),
		Command:          stringSliceFromInterface(asSlice(values["command"])),
		Args:             stringSliceFromInterface(asSlice(values["args"])),
		Env:              stringMapFromInterface(asMap(values["env"])),
		GPUCount:         intValue(values["gpuCount"]),
		GPUType:          stringValue(values["gpuType"]),
		MaxPrice:         stringValue(values["maxPrice"]),
		Provider:         stringValue(values["provider"]),
		RegionHint:       stringValue(values["regionHint"]),
		RestartPolicy:    stringValue(values["restartPolicy"]),
		ImagePullSecrets: stringSliceFromInterface(asSlice(values["imagePullSecrets"])),
		Ports:            portsFromSlice(asSlice(values["ports"])),
	}
}

func placementToMap(placement core.Placement) map[string]interface{} {
	return map[string]interface{}{
		"provider": placement.Provider,
		"region":   placement.Region,
		"gpuType":  placement.GPUType,
	}
}

func placementFromMap(values map[string]interface{}) core.Placement {
	return core.Placement{
		Provider: stringValue(values["provider"]),
		Region:   stringValue(values["region"]),
		GPUType:  stringValue(values["gpuType"]),
	}
}

func attemptsToSlice(attempts []core.Attempt) []interface{} {
	if len(attempts) == 0 {
		return nil
	}
	out := make([]interface{}, 0, len(attempts))
	for _, attempt := range attempts {
		out = append(out, map[string]interface{}{
			"provider": attempt.Provider,
			"region":   attempt.Region,
			"reason":   attempt.Reason,
			"message":  attempt.Message,
		})
	}
	return out
}

func attemptsFromSlice(values []interface{}) []core.Attempt {
	if len(values) == 0 {
		return nil
	}
	out := make([]core.Attempt, 0, len(values))
	for _, value := range values {
		item := asMap(value)
		out = append(out, core.Attempt{
			Provider: stringValue(item["provider"]),
			Region:   stringValue(item["region"]),
			Reason:   stringValue(item["reason"]),
			Message:  stringValue(item["message"]),
		})
	}
	return out
}

func portsToSlice(ports []core.Port) []interface{} {
	if len(ports) == 0 {
		return nil
	}
	out := make([]interface{}, 0, len(ports))
	for _, port := range ports {
		out = append(out, map[string]interface{}{
			"name":          port.Name,
			"containerPort": int64(port.ContainerPort),
			"protocol":      port.Protocol,
		})
	}
	return out
}

func portsFromSlice(values []interface{}) []core.Port {
	if len(values) == 0 {
		return nil
	}
	out := make([]core.Port, 0, len(values))
	for _, value := range values {
		item := asMap(value)
		out = append(out, core.Port{
			Name:          stringValue(item["name"]),
			ContainerPort: intValue(item["containerPort"]),
			Protocol:      stringValue(item["protocol"]),
		})
	}
	return out
}

func stringSliceToInterface(values []string) []interface{} {
	if len(values) == 0 {
		return nil
	}
	out := make([]interface{}, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func stringSliceFromInterface(values []interface{}) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, stringValue(value))
	}
	return out
}

func stringMapToInterface(values map[string]string) map[string]interface{} {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func stringMapFromInterface(values map[string]interface{}) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = stringValue(value)
	}
	return out
}

func asMap(value interface{}) map[string]interface{} {
	if value == nil {
		return nil
	}
	if out, ok := value.(map[string]interface{}); ok {
		return out
	}
	return nil
}

func asSlice(value interface{}) []interface{} {
	if value == nil {
		return nil
	}
	if out, ok := value.([]interface{}); ok {
		return out
	}
	return nil
}

func stringValue(value interface{}) string {
	if value == nil {
		return ""
	}
	if out, ok := value.(string); ok {
		return out
	}
	return fmt.Sprint(value)
}

func intValue(value interface{}) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func boolValue(value interface{}) bool {
	if out, ok := value.(bool); ok {
		return out
	}
	return false
}

func removeString(values []string, remove string) []string {
	out := values[:0]
	for _, value := range values {
		if value != remove {
			out = append(out, value)
		}
	}
	return out
}
