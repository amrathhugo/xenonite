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

package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TemplateSource identifies the Proxmox VM template to clone from.
type TemplateSource struct {
	// +required
	Node string `json:"node"`

	// +required
	// +kubebuilder:validation:Minimum=100
	VMID int `json:"vmid"`
}

// CloudInitConfig describes the cloud-init user-data injected into cloned VMs.
type CloudInitConfig struct {
	// UserData is a Go text/template rendered per node. Available fields:
	// {{ .NodeName }} {{ .Server }} {{ .Token }}.
	// +required
	UserData string `json:"userData"`

	// +optional
	Storage string `json:"storage,omitempty"`

	// +optional
	// +kubebuilder:default=ide2
	Device string `json:"device,omitempty"`
}

// SecretKeyRef points to a single key inside a Secret that lives in the
// operator's own namespace (it is cluster-global config, not per-tenant).
type SecretKeyRef struct {
	// +required
	Name string `json:"name"`

	// +required
	Key string `json:"key"`
}

// BootstrapConfig carries the cluster join parameters for new nodes.
type BootstrapConfig struct {
	// Server is the cluster URL passed to the agent, e.g. https://10.0.0.1:6443.
	// +required
	Server string `json:"server"`

	// TokenSecretRef sources the join token; injected into cloud-init as {{ .Token }}.
	// +required
	TokenSecretRef SecretKeyRef `json:"tokenSecretRef"`
}

// ProxmoxProviderSpec defines the desired state of ProxmoxProvider
type ProxmoxProviderSpec struct {
	// +required
	Endpoint string `json:"endpoint"`

	// +optional
	Insecure bool `json:"insecure,omitempty"`

	// +required
	CredentialsSecretRef corev1.SecretReference `json:"credentialsSecretRef"`

	// +required
	// +kubebuilder:validation:MinItems=1
	Nodes []string `json:"nodes"`

	// +optional
	Pool string `json:"pool,omitempty"`

	// +required
	SourceTemplate TemplateSource `json:"sourceTemplate"`

	// +required
	Storage string `json:"storage"`

	// +optional
	NetworkBridge string `json:"networkBridge,omitempty"`

	// +optional
	Tags []string `json:"tags,omitempty"`

	// +required
	CloudInit CloudInitConfig `json:"cloudInit"`

	// +required
	Bootstrap BootstrapConfig `json:"bootstrap"`
}

// ProxmoxProviderStatus defines the observed state of ProxmoxProvider.
type ProxmoxProviderStatus struct {
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// ProxmoxProvider is the Schema for the proxmoxproviders API
type ProxmoxProvider struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec ProxmoxProviderSpec `json:"spec"`

	// +optional
	Status ProxmoxProviderStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ProxmoxProviderList contains a list of ProxmoxProvider
type ProxmoxProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ProxmoxProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ProxmoxProvider{}, &ProxmoxProviderList{})
		return nil
	})
}
