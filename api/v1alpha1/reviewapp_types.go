/*
Copyright 2024.

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

package v1alpha1

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type Deployments struct {
	Name                string `json:"name"`
	TargetContainerName string `json:"targetContainerName"`
	TargetContainerPort int32  `json:"targetContainerPort"`

	// HostTemplates must contain at least {{.BranchName}}
	HostTemplates []string `json:"hostTemplates"`

	// generateEmbeddedObjectMeta must be set for this to work
	Template corev1.PodTemplateSpec `json:"template"`

	// +optional
	Strategy appsv1.DeploymentStrategy `json:"strategy,omitempty"`

	// +optional
	MinReadySeconds int32 `json:"minReadySeconds,omitempty"`

	// +optional
	ProgressDeadlineSeconds *int32 `json:"progressDeadlineSeconds,omitempty"`
}

// ReviewAppSpec defines the desired state of ReviewApp
type ReviewAppSpec struct {
	Deployments []Deployments `json:"deployments"`
}

// ReviewAppStatus defines the observed state of ReviewApp
type ReviewAppStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// A list of pointers to currently running jobs.
	// +optional
	Active []corev1.ObjectReference `json:"active,omitempty"`

	// Information when was the last time the job was successfully scheduled.
	// +optional
	LastUpdate *metav1.Time `json:"lastUpdate,omitempty"`

	// +optional
	ActiveApps []string `json:"activeApps"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rapp

// ReviewApp is the Schema for the reviewapps API
type ReviewApp struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReviewAppSpec   `json:"spec,omitempty"`
	Status ReviewAppStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ReviewAppList contains a list of ReviewApp
type ReviewAppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReviewApp `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReviewApp{}, &ReviewAppList{})
}
