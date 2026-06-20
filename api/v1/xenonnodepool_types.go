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

// XenonNodeClaimTemplate is the template used to create XenonNodeClaims.
type XenonNodeClaimTemplate struct {
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// +required
	Spec XenonNodeClaimSpec `json:"spec"`
}

// XenonNodePoolSpec defines the desired state of XenonNodePool
type XenonNodePoolSpec struct {
	// +required
	Template XenonNodeClaimTemplate `json:"template"`

	// +optional
	Limits corev1.ResourceList `json:"limits,omitempty"`

	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Weight *int32 `json:"weight,omitempty"`
}

// XenonNodePoolStatus defines the observed state of XenonNodePool.
type XenonNodePoolStatus struct {
	// +optional
	Resources corev1.ResourceList `json:"resources,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// XenonNodePool is the Schema for the xenonnodepools API
type XenonNodePool struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec XenonNodePoolSpec `json:"spec"`

	// +optional
	Status XenonNodePoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// XenonNodePoolList contains a list of XenonNodePool
type XenonNodePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []XenonNodePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &XenonNodePool{}, &XenonNodePoolList{})
		return nil
	})
}
