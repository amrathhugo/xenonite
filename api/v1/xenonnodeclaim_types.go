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

// XenonNodeClaimSpec defines the desired state of XenonNodeClaim
type XenonNodeClaimSpec struct {
	// +required
	NodePoolRef corev1.LocalObjectReference `json:"nodePoolRef"`

	// +optional
	// +listType=atomic
	Requirements []corev1.NodeSelectorRequirement `json:"requirements,omitempty"`

	// +optional
	Resources corev1.ResourceList `json:"resources,omitempty"`
}

// XenonNodeClaimStatus defines the observed state of XenonNodeClaim.
type XenonNodeClaimStatus struct {
	// +optional
	ProviderID string `json:"providerID,omitempty"`

	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// +optional
	Capacity corev1.ResourceList `json:"capacity,omitempty"`

	// +optional
	Allocatable corev1.ResourceList `json:"allocatable,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// XenonNodeClaim is the Schema for the xenonnodeclaims API
type XenonNodeClaim struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec XenonNodeClaimSpec `json:"spec"`

	// +optional
	Status XenonNodeClaimStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// XenonNodeClaimList contains a list of XenonNodeClaim
type XenonNodeClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []XenonNodeClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &XenonNodeClaim{}, &XenonNodeClaimList{})
		return nil
	})
}
