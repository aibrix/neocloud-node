package core

import "errors"

var ErrBindingNotFound = errors.New("binding not found")

type BindingPhase string

const (
	BindingPending  BindingPhase = "Pending"
	BindingRunning  BindingPhase = "Running"
	BindingDeleting BindingPhase = "Deleting"
	BindingDeleted  BindingPhase = "Deleted"
	BindingFailed   BindingPhase = "Failed"
)

type WorkloadRequest struct {
	Image      string
	Command    []string
	Args       []string
	Env        map[string]string
	GPUCount   int
	GPUType    string
	MaxPrice   string
	Provider   string
	RegionHint string
}

type Placement struct {
	Provider string
	Region   string
	GPUType  string
}

type Attempt struct {
	Provider string
	Region   string
	Reason   string
	Message  string
}

type Binding struct {
	Namespace   string
	Name        string
	WorkloadUID string
	Request     WorkloadRequest
	RemoteID    string
	Phase       BindingPhase
	Placement   Placement
	Attempts    []Attempt
	Deleting    bool
	LastError   string
}
