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

// Package proxmox implements provider.Provider on top of luthermonson/go-proxmox.
package proxmox

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/luthermonson/go-proxmox"

	xv1 "github.com/amrathhugo/xenonite/api/v1"
	"github.com/amrathhugo/xenonite/internal/provider"
)

const (
	// providerIDScheme is the prefix of every providerID this backend mints:
	// proxmox://<node>/<vmid>.
	providerIDScheme = "proxmox://"

	// taskTimeoutSecs bounds how long we wait for a single PVE task (clone,
	// config, start, stop, delete) to finish before returning an error so the
	// controller can retry with backoff.
	taskTimeoutSecs = 600
)


// Provider is the Proxmox-backed implementation of provider.Provider. One is
// constructed per ProxmoxProvider CR; it captures that CR's spec so per-node
// Create calls only carry node-specific values.
type Provider struct {
	client *proxmox.Client
	spec   xv1.ProxmoxProviderSpec
}

var _ provider.Provider = (*Provider)(nil)

// New builds a Provider from a ProxmoxProvider spec and its API-token creds.
func New(spec xv1.ProxmoxProviderSpec, creds *proxmox.Credentials) *Provider {
	httpClient := &http.Client{}
	if spec.Insecure {
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 — opt-in via spec.Insecure
		}
	}
	client := proxmox.NewClient(spec.Endpoint,
		proxmox.WithHTTPClient(httpClient),
		proxmox.WithCredentials(creds),
	)
	return &Provider{client: client, spec: spec}
}

// Create clones the source template, applies sizing + cloud-init, and starts the
// VM. The returned providerID is proxmox://<targetNode>/<vmid>.
func (p *Provider) Create(ctx context.Context, req provider.CreateRequest) (string, error) {
	target := p.targetNode()

	// Reserve a cluster-unique VMID.
	cluster, err := p.client.Cluster(ctx)
	if err != nil {
		return "", fmt.Errorf("get cluster: %w", err)
	}
	newID, err := cluster.NextID(ctx)
	if err != nil {
		return "", fmt.Errorf("allocate vmid: %w", err)
	}

	// Locate and clone the template.
	tmplNode, err := p.client.Node(ctx, p.spec.SourceTemplate.Node)
	if err != nil {
		return "", fmt.Errorf("get template node %q: %w", p.spec.SourceTemplate.Node, err)
	}
	tmpl, err := tmplNode.VirtualMachine(ctx, p.spec.SourceTemplate.VMID)
	if err != nil {
		return "", fmt.Errorf("get template vm %d: %w", p.spec.SourceTemplate.VMID, err)
	}
	_, cloneTask, err := tmpl.Clone(ctx, &proxmox.VirtualMachineCloneOptions{
		NewID:   newID,
		Name:    req.Name,
		Target:  target,
		Storage: p.spec.Storage,
		Pool:    p.spec.Pool,
		Full:    true,
	})
	if err != nil {
		return "", fmt.Errorf("clone template: %w", err)
	}
	if err := cloneTask.WaitFor(ctx, taskTimeoutSecs); err != nil {
		return "", fmt.Errorf("wait for clone: %w", err)
	}

	providerID := newProviderID(target, newID)

	// From here the VM exists; failures return providerID so the controller can
	// record it and drive cleanup/retry instead of leaking the VM.
	node, err := p.client.Node(ctx, target)
	if err != nil {
		return providerID, fmt.Errorf("get target node %q: %w", target, err)
	}
	vm, err := node.VirtualMachine(ctx, newID)
	if err != nil {
		return providerID, fmt.Errorf("get cloned vm %d: %w", newID, err)
	}

	if err := p.configure(ctx, vm, req); err != nil {
		return providerID, err
	}
	if err := vm.CloudInit(ctx, p.device(), req.CloudInitUserData, "", "", "",
		proxmox.WithCloudInitStorage(p.cloudInitStorage())); err != nil {
		return providerID, fmt.Errorf("apply cloud-init: %w", err)
	}
	startTask, err := vm.Start(ctx)
	if err != nil {
		return providerID, fmt.Errorf("start vm: %w", err)
	}
	if err := startTask.WaitFor(ctx, taskTimeoutSecs); err != nil {
		return providerID, fmt.Errorf("wait for start: %w", err)
	}
	return providerID, nil
}

// configure applies sizing and tags. Skips fields the request leaves at zero so
// the template defaults stand.
func (p *Provider) configure(ctx context.Context, vm *proxmox.VirtualMachine, req provider.CreateRequest) error {
	var opts []proxmox.VirtualMachineOption
	if req.Cores > 0 {
		opts = append(opts, proxmox.VirtualMachineOption{Name: "cores", Value: req.Cores})
	}
	if req.MemoryMB > 0 {
		opts = append(opts, proxmox.VirtualMachineOption{Name: "memory", Value: req.MemoryMB})
	}
	if len(p.spec.Tags) > 0 {
		opts = append(opts, proxmox.VirtualMachineOption{Name: "tags", Value: strings.Join(p.spec.Tags, ";")})
	}
	if len(opts) == 0 {
		return nil
	}
	task, err := vm.Config(ctx, opts...)
	if err != nil {
		return fmt.Errorf("configure vm: %w", err)
	}
	if err := task.WaitFor(ctx, taskTimeoutSecs); err != nil {
		return fmt.Errorf("wait for configure: %w", err)
	}
	return nil
}

// Delete stops (if needed) and purges the VM. A missing VM is treated as success.
func (p *Provider) Delete(ctx context.Context, providerID string) error {
	node, vmid, err := parseProviderID(providerID)
	if err != nil {
		return err
	}
	n, err := p.client.Node(ctx, node)
	if err != nil {
		if proxmox.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get node %q: %w", node, err)
	}
	vm, err := n.VirtualMachine(ctx, vmid)
	if err != nil {
		if proxmox.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get vm %d: %w", vmid, err)
	}

	if vm.Status == "running" {
		stopTask, err := vm.Stop(ctx)
		if err != nil {
			return fmt.Errorf("stop vm %d: %w", vmid, err)
		}
		if err := stopTask.WaitFor(ctx, taskTimeoutSecs); err != nil {
			return fmt.Errorf("wait for stop: %w", err)
		}
	}

	delTask, err := vm.Delete(ctx, &proxmox.VirtualMachineDeleteOptions{Purge: true, DestroyUnreferencedDisks: true})
	if err != nil {
		return fmt.Errorf("delete vm %d: %w", vmid, err)
	}
	if err := delTask.WaitFor(ctx, taskTimeoutSecs); err != nil {
		return fmt.Errorf("wait for delete: %w", err)
	}
	return nil
}

// Get returns the instance, or (nil, nil) if it no longer exists.
func (p *Provider) Get(ctx context.Context, providerID string) (*provider.Instance, error) {
	node, vmid, err := parseProviderID(providerID)
	if err != nil {
		return nil, err
	}
	n, err := p.client.Node(ctx, node)
	if err != nil {
		if proxmox.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get node %q: %w", node, err)
	}
	vm, err := n.VirtualMachine(ctx, vmid)
	if err != nil {
		if proxmox.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get vm %d: %w", vmid, err)
	}
	return &provider.Instance{ProviderID: providerID, Status: vm.Status}, nil
}

// targetNode picks the Proxmox node to place the VM on. First configured node
func (p *Provider) targetNode() string {
	if len(p.spec.Nodes) > 0 {
		return p.spec.Nodes[0]
	}
	return p.spec.SourceTemplate.Node
}

func (p *Provider) device() string {
	if p.spec.CloudInit.Device != "" {
		return p.spec.CloudInit.Device
	}
	return "ide2"
}

func (p *Provider) cloudInitStorage() string {
	if p.spec.CloudInit.Storage != "" {
		return p.spec.CloudInit.Storage
	}
	return p.spec.Storage
}

func newProviderID(node string, vmid int) string {
	return fmt.Sprintf("%s%s/%d", providerIDScheme, node, vmid)
}

func parseProviderID(providerID string) (node string, vmid int, err error) {
	trimmed := strings.TrimPrefix(providerID, providerIDScheme)
	if trimmed == providerID {
		return "", 0, fmt.Errorf("providerID %q is not a proxmox id", providerID)
	}
	node, idStr, ok := strings.Cut(trimmed, "/")
	if !ok || node == "" || idStr == "" {
		return "", 0, fmt.Errorf("providerID %q is malformed, want %s<node>/<vmid>", providerID, providerIDScheme)
	}
	vmid, err = strconv.Atoi(idStr)
	if err != nil {
		return "", 0, fmt.Errorf("providerID %q has non-numeric vmid: %w", providerID, err)
	}
	return node, vmid, nil
}
