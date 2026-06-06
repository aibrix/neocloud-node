package core

import (
	"errors"
	"time"
)

var ErrBindingNotFound = errors.New("binding not found")

type BindingPhase string

const (
	BindingPending   BindingPhase = "Pending"
	BindingRunning   BindingPhase = "Running"
	BindingSucceeded BindingPhase = "Succeeded"
	BindingDeleting  BindingPhase = "Deleting"
	BindingDeleted   BindingPhase = "Deleted"
	BindingFailed    BindingPhase = "Failed"
)

type WorkloadRequest struct {
	Image            string
	Command          []string
	Args             []string
	Env              map[string]string
	GPUCount         int
	GPUType          string
	GPUMemory        string
	MaxPrice         string
	MarketType       string
	Provider         string
	RegionHint       string
	RestartPolicy    string
	ImagePullSecrets []string
	Ports            []Port
	ProviderHints    map[string]string
}

type Port struct {
	Name          string
	ContainerPort int
	Protocol      string
}

type Placement struct {
	Provider string
	Region   string
	GPUType  string
}

type CostStatus struct {
	HourlyUSD  float64
	Source     string
	ObservedAt time.Time
}

type Attempt struct {
	Provider        string
	Region          string
	Class           ErrorClass
	Reason          string
	Message         string
	Retryable       bool
	FallbackAllowed bool
	Timestamp       time.Time
}

type ConditionType string

const (
	ConditionReady           ConditionType = "Ready"
	ConditionUnsupportedSpec ConditionType = "UnsupportedSpec"
	ConditionProviderError   ConditionType = "ProviderError"
	ConditionCapacity        ConditionType = "Capacity"
	ConditionAuth            ConditionType = "Auth"
	ConditionDeleteFailure   ConditionType = "DeleteFailure"
	ConditionRemoteDeleted   ConditionType = "RemoteDeleted"
)

type ConditionStatus string

const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

type BindingCondition struct {
	Type               ConditionType
	Status             ConditionStatus
	Reason             string
	Message            string
	LastTransitionTime time.Time
}

type Binding struct {
	Namespace   string
	Name        string
	WorkloadUID string
	Request     WorkloadRequest
	RemoteID    string
	Phase       BindingPhase
	Placement   Placement
	Cost        CostStatus
	Attempts    []Attempt
	Conditions  []BindingCondition
	Deleting    bool
	LastError   string
}

func FindCondition(conditions []BindingCondition, conditionType ConditionType) *BindingCondition {
	for index := range conditions {
		if conditions[index].Type == conditionType {
			return &conditions[index]
		}
	}
	return nil
}

func SetCondition(binding *Binding, condition BindingCondition) {
	if binding == nil {
		return
	}
	if condition.LastTransitionTime.IsZero() {
		condition.LastTransitionTime = time.Now().UTC()
	}
	for index := range binding.Conditions {
		if binding.Conditions[index].Type == condition.Type {
			if binding.Conditions[index].Status == condition.Status && !binding.Conditions[index].LastTransitionTime.IsZero() {
				condition.LastTransitionTime = binding.Conditions[index].LastTransitionTime
			}
			binding.Conditions[index] = condition
			return
		}
	}
	binding.Conditions = append(binding.Conditions, condition)
}
