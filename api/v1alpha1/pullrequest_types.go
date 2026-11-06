package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PullRequestSpec struct {
	ImageName string `json:"imageName,omitempty"`

	BranchName string `json:"branchName,omitempty"`

	// The parent ReviewAppConfig
	ReviewAppConfigRef string `json:"reviewAppRef,omitempty"`

	// Deployments overrides the ReviewAppConfig for deployments of this review app, like to point
	// it at the review app of another repository. Deploys keep them, as the webhook only changes
	// the image
	// +optional
	// +listType=map
	// +listMapKey=name
	Deployments []DeploymentOverrides `json:"deployments,omitempty"`
}

// DeploymentOverrides overrides the ReviewAppConfig for one deployment of a review app
type DeploymentOverrides struct {
	// Name is the name of the deployment in the ReviewAppConfig
	Name string `json:"name"`

	// Env sets environment variables of the target container, replacing those of the same name
	// in the ReviewAppConfig. Changing them restarts the review app
	// +optional
	// +listType=map
	// +listMapKey=name
	Env []EnvVar `json:"env,omitempty"`
}

// EnvVar is an environment variable with a value. Values from Secrets and ConfigMaps can only
// be set in the ReviewAppConfig, so that whoever can edit PullRequests can't read them through
// a review app
type EnvVar struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +optional
	Value string `json:"value,omitempty"`
}

// PullRequestStatus defines the observed state of PullRequest
type PullRequestStatus struct {
	// +optional
	DeployedAt metav1.Time `json:"deployedAt,omitempty"`
	// +optional
	DeployedBy string `json:"deployedBy,omitempty"`
	// +optional
	RepositoryURL string `json:"repositoryUrl,omitempty"`
	// +optional
	PullRequestURL string `json:"pullRequestUrl,omitempty"`
	// +optional
	PullRequestNumber int `json:"pullRequestNumber,omitempty"`

	// URL is the URL of the review app, from the first host template of its first deployment. The
	// URLs of every deployment are in their hostnames
	// +optional
	URL string `json:"url,omitempty"`

	Deployments map[string]*DeploymentStatus `json:"activeDeployments"`

	// Conditions report whether the controller could create and update the review app
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ConditionReconciled is the condition type that reports whether the controller could
// create and update the deployments of the review app, and why not
const ConditionReconciled = "Reconciled"

type DeploymentStatus struct {
	// LastActive was the last activity recorded by earlier versions, which record the visits in a
	// Lease named like the deployment now. The controller moves it to the Lease and clears it
	// +optional
	LastActive metav1.Time `json:"lastActive,omitempty"`

	// IsActive was whether the deployment ran in earlier versions, where it runs while its Lease
	// is held now. The controller clears it with LastActive
	// +optional
	IsActive bool `json:"isActive,omitempty"`

	// +optional
	Hostnames []string `json:"hostnames"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pr
// +kubebuilder:printcolumn:name="Branch",type=string,JSONPath=`.spec.branchName`
// +kubebuilder:printcolumn:name="Reconciled",type=string,JSONPath=`.status.conditions[?(@.type=="Reconciled")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Reconciled")].reason`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Message",type=string,priority=1,JSONPath=`.status.conditions[?(@.type=="Reconciled")].message`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PullRequest is the Schema for the pullrequests API
type PullRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PullRequestSpec   `json:"spec,omitempty"`
	Status PullRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PullRequestList contains a list of PullRequest
type PullRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PullRequest `json:"items"`
}
