package reviewapp

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	racwilliamnuv1alpha1 "github.com/wille/rac/api/v1alpha1"
	"github.com/wille/rac/internal/utils"
	"k8s.io/apimachinery/pkg/types"
)

func DeletePullRequestByName(pullRequestName string) error {
	c, err := utils.GetKubernetesClient()
	if err != nil {
		return err
	}

	var pr racwilliamnuv1alpha1.PullRequest
	if err := c.Get(context.TODO(), types.NamespacedName{
		Name:      pullRequestName,
		Namespace: "default",
	}, &pr); err != nil {
		return err
	}

	if err := c.Delete(context.TODO(), &pr); err != nil {
		return err
	}

	return nil
}

func CreateOrUpdatePullRequest(reviewApp *racwilliamnuv1alpha1.ReviewApp, prName string, spec racwilliamnuv1alpha1.PullRequestSpec) error {
	c, err := utils.GetKubernetesClient()
	if err != nil {
		return err
	}

	pr := racwilliamnuv1alpha1.PullRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      prName,
			Labels:    reviewApp.Labels,
			Namespace: reviewApp.Namespace,
		},
		Spec: spec,
	}

	existingPr := racwilliamnuv1alpha1.PullRequest{}
	if err := c.Get(context.TODO(), types.NamespacedName{
		Name:      prName,
		Namespace: reviewApp.Namespace,
	}, &existingPr); err != nil {
		// Create the PullRequest if it is not found
		if apierrors.IsNotFound(err) {
			return c.Create(context.TODO(), &pr)
		}

		return err
	}

	patch := client.MergeFrom(existingPr.DeepCopy())
	existingPr.Spec = spec

	// Update the PullRequest if it is found
	if err := c.Patch(context.TODO(), &existingPr, patch); err != nil {
		return err
	}

	fmt.Println("Updated", "spec", spec)

	return nil

}
