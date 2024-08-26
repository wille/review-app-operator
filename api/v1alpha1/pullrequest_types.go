package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PullRequestSpec struct {
	ImageName string `json:"imageName,omitempty"`

	BranchName string `json:"branchName,omitempty"`

	// The parent ReviewAppConfig
	ReviewAppConfigRef string `json:"reviewAppRef,omitempty"`
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

	Deployments map[string]*DeploymentStatus `json:"activeDeployments"`
}

type DeploymentStatus struct {
	// +optional
	LastActive metav1.Time `json:"lastActive,omitempty"`

	// +optional
	IsActive bool `json:"isActive"`

	// +optional
	Hostnames []string `json:"hostnames"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pr

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

func init() {
	SchemeBuilder.Register(&PullRequest{}, &PullRequestList{})
}
