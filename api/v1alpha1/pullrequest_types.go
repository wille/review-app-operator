package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PullRequestSpec struct {
	ImageName string `json:"imageName,omitempty"`

	BranchName string `json:"branchName,omitempty"`

	// The parent ReviewApp
	ReviewAppRef string `json:"reviewAppRef,omitempty"`
}

// PullRequestStatus defines the observed state of PullRequest
type PullRequestStatus struct {
	DeployedAt        metav1.Time `json:"deployedAt"`
	DeployedBy        string      `json:"deployedBy"`
	RepositoryURL     string      `json:"repositoryUrl"`
	PullRequestURL    string      `json:"pullRequestUrl"`
	PullRequestNumber int         `json:"pullRequestNumber"`
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
