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

// Deployments is a deployment of the review apps. Its target container runs the image of the pull request
type Deployments struct {
	// Name is the name of the deployment, which must be different for each deployment
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name,omitempty"`

	// TargetContainerName is the container of the template that runs the image of the pull request.
	// The API server can't check that the template has it, as the rule would be too costly for
	// the unlimited containers of a pod, so deploys fail when it doesn't
	// +kubebuilder:validation:MaxLength=63
	TargetContainerName string `json:"targetContainerName"`

	// TargetContainerPort is the port of the target container that the forwarder sends requests to
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	TargetContainerPort int32 `json:"targetContainerPort"`

	// +optional
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas,omitempty"`
	// +optional
	StartOnDeploy bool `json:"startOnDeploy,omitempty"`

	// ScaleDownAfter overrides the scaleDownAfter of the ReviewAppConfig for this deployment
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^(0|([0-9]+(\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+)$`
	ScaleDownAfter metav1.Duration `json:"scaleDownAfter,omitempty"`

	// HostTemplates must contain {{.BranchName}} or {{.RandomID}}, and may only use the
	// template variables and lowercase hostname characters. Keep the pattern in step with
	// utils.GetHostnameFromTemplate
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Pattern=`^([a-z0-9.-]|\{\{\.(ReviewAppConfig|BranchName|DeploymentName|PullRequestNumber|RandomID)\}\})*\{\{\.(BranchName|RandomID)\}\}([a-z0-9.-]|\{\{\.(ReviewAppConfig|BranchName|DeploymentName|PullRequestNumber|RandomID)\}\})*$`
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

// ReviewAppConfigSpec defines the desired state of ReviewAppConfig
type ReviewAppConfigSpec struct {
	// +optional
	StartOnDeploy bool `json:"startOnDeploy,omitempty"`

	// ConnectionTimeout is how long the forwarder waits for a review app to start before it asks
	// browsers to reload, and answers other requests with 503 Service Unavailable. It overrides
	// the --connection-timeout of the forwarder
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^(0|([0-9]+(\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+)$`
	ConnectionTimeout metav1.Duration `json:"connectionTimeout,omitempty"`

	// ScaleDownAfter scales down the deployments of a review app that receive no requests for
	// this long. Deployments are not scaled down if it is not set
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^(0|([0-9]+(\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+)$`
	ScaleDownAfter metav1.Duration `json:"scaleDownAfter,omitempty"`

	// DeleteAfter deletes the review app of a pull request that has neither been deployed nor
	// received traffic for this long, like when the webhook could not delete it as the pull
	// request closed. The next deploy creates it again. Review apps are not deleted if it is not set
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^(0|([0-9]+(\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+)$`
	DeleteAfter metav1.Duration `json:"deleteAfter,omitempty"`

	// Repositories are the GitHub repositories, like octo-org/octo-repo, whose pull request
	// workflows can deploy and delete these review apps with a GitHub Actions ID token, without
	// the webhook secret
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`
	Repositories []string `json:"repositories,omitempty"`

	// Deployments are the deployments of each review app. Their names must be different, as they
	// are part of the names of the Deployments
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:XValidation:rule="self.all(d, self.filter(e, (has(e.name) ? e.name : '') == (has(d.name) ? d.name : '')).size() == 1)",message="deployments must have different names"
	Deployments []Deployments `json:"deployments"`
}

// ReviewAppConfigStatus defines the observed state of ReviewAppConfig
type ReviewAppConfigStatus struct {
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=reviewapp

// ReviewAppConfig is the Schema for the reviewapps API
type ReviewAppConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReviewAppConfigSpec   `json:"spec,omitempty"`
	Status ReviewAppConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ReviewAppConfigList contains a list of ReviewAppConfig
type ReviewAppConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReviewAppConfig `json:"items"`
}
