package utils

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubectl/pkg/polymorphichelpers"

	appsv1 "k8s.io/api/apps/v1"
)

var statusViewer = polymorphichelpers.DeploymentStatusViewer{}

// GetDeploymentStatus returns the status of a deployment like `kubectl rollout status`
func GetDeploymentStatus(d *appsv1.Deployment) (string, bool, error) {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&d)
	if err != nil {
		return "", false, err
	}

	status, done, err := statusViewer.Status(&unstructured.Unstructured{Object: obj}, 0)
	if err != nil {
		return "", false, err
	}

	return status, done, err
}
