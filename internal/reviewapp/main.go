package reviewapp

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"k8s.io/kubectl/pkg/polymorphichelpers"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racwilliamnuv1alpha1 "github.com/wille/rac/api/v1alpha1"
	"github.com/wille/rac/internal/utils"
	"k8s.io/apimachinery/pkg/types"
)

func DeletePullRequestByName(key types.NamespacedName) error {
	c, err := utils.GetKubernetesClient()
	if err != nil {
		return err
	}

	var pr racwilliamnuv1alpha1.PullRequest
	if err := c.Get(context.TODO(), key, &pr); err != nil {
		return err
	}

	if err := c.Delete(context.TODO(), &pr); err != nil {
		return err
	}

	return nil
}

func CreateOrUpdatePullRequest(reviewApp *racwilliamnuv1alpha1.ReviewApp, key types.NamespacedName, spec racwilliamnuv1alpha1.PullRequestSpec) (*racwilliamnuv1alpha1.PullRequest, error) {
	c, err := utils.GetKubernetesClient()
	if err != nil {
		return nil, err
	}

	pr := racwilliamnuv1alpha1.PullRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Labels:    reviewApp.Labels,
			Namespace: key.Namespace,
		},
		Spec: spec,
	}

	if err := c.Get(context.TODO(), key, &pr); err != nil {
		// Create the PullRequest if it is not found
		if apierrors.IsNotFound(err) {
			if err := c.Create(context.TODO(), &pr); err != nil {

				return nil, err
			}
		} else {
			return nil, err
		}
	} else {
		patch := client.MergeFrom(pr.DeepCopy())
		pr.Spec = spec

		// Update the PullRequest if it is found
		if err := c.Patch(context.TODO(), &pr, patch); err != nil {
			return nil, err
		}
	}

	fmt.Println("Updated", "spec", spec)

	time.Sleep(5 * time.Second)

	attempts := 0
	for {
		bothDone := true

		for _, deploymentSpec := range reviewApp.Spec.Deployments {
			deploymentName := utils.GetResourceName(pr.Name, deploymentSpec.Name)

			var deployment = appsv1.Deployment{}
			if err := c.Get(context.TODO(), types.NamespacedName{
				Name:      deploymentName,
				Namespace: key.Namespace,
			}, &deployment); err != nil {
				return nil, err
			}

			statusViewer := polymorphichelpers.DeploymentStatusViewer{}

			obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&deployment)
			if err != nil {
				return nil, err
			}

			status, done, err := statusViewer.Status(&unstructured.Unstructured{Object: obj}, 0)
			if err != nil {
				return nil, err
			}
			if !done {
				bothDone = false
			}

			fmt.Println("Deployment status", status, done, err)
		}

		if bothDone {
			break
		}

		attempts++

		if attempts > 30 {
			return nil, fmt.Errorf("Timeout reached")
		}

		time.Sleep(1 * time.Second)
	}

	return &pr, nil
}
