/*
Copyright 2026 mohammedamrath.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package provider abstracts the infrastructure that backs a XenonNodeClaim so
// the controllers can provision nodes without depending on a specific cloud or
// hypervisor. Proxmox is the first implementation; bare-metal can be added by
// satisfying the same interface.
package provider

import "context"

// CreateRequest is the per-node input to Create. Provider-wide configuration
// (template, storage, network, credentials) is bound when the Provider is
// constructed, not passed here.
type CreateRequest struct {
	// Name is the VM/node name; the controller uses the XenonNodeClaim name.
	Name string
	// Cores / MemoryMB / DiskGB override the template sizing when non-zero.
	Cores    int
	MemoryMB int
	DiskGB   int
	// CloudInitUserData is the fully rendered cloud-init document (join command
	// already injected by the controller).
	CloudInitUserData string
}

// Instance is the observed state of a backing machine.
type Instance struct {
	ProviderID string
	// Status is the provider-native power state (e.g. "running", "stopped").
	Status string
}

// Provider is the contract every infrastructure backend implements. All methods
// must be safe to call repeatedly (idempotent) because the controllers retry.
type Provider interface {
	// Create provisions a machine and returns its stable providerID. Callers may
	// retry on error; implementations should avoid creating duplicates.
	Create(ctx context.Context, req CreateRequest) (providerID string, err error)

	// Delete removes the machine identified by providerID. Deleting a machine
	// that no longer exists must succeed (no error).
	Delete(ctx context.Context, providerID string) error

	// Get returns the instance for providerID, or (nil, nil) when it does not
	// exist. Used to confirm creation and to verify deletion completed.
	Get(ctx context.Context, providerID string) (*Instance, error)
}
