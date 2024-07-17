package reviewapp

import (
	"context"
	"fmt"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"k8s.io/kubectl/pkg/polymorphichelpers"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/utils"
	"k8s.io/apimachinery/pkg/types"
)

func DeletePullRequestByName(key types.NamespacedName) error {
	c, err := utils.GetKubernetesClient()
	if err != nil {
		return err
	}

	var pr reviewapps.PullRequest
	if err := c.Get(context.TODO(), key, &pr); err != nil {
		return err
	}

	if err := c.Delete(context.TODO(), &pr); err != nil {
		return err
	}

	return nil
}

func writeFlush(w http.ResponseWriter, s string) {
	w.Write([]byte(s))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// CreateOrUpdatePullRequest ensures that the desired pull request exists and is up to date
// and streams the status of the deployments to the response writer
func CreateOrUpdatePullRequest(
	reviewApp *reviewapps.ReviewApp,
	key types.NamespacedName,
	spec reviewapps.PullRequestSpec,
	w http.ResponseWriter,
) (*reviewapps.PullRequest, error) {
	log := log.Log.WithName("webhooks")

	c, err := utils.GetKubernetesClient()
	if err != nil {
		return nil, err
	}

	pr := reviewapps.PullRequest{
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

			w.WriteHeader(http.StatusCreated)
			writeFlush(w, fmt.Sprintf("Created pull request \"%s\" for branch \"%s\"\n", pr.Name, pr.Spec.BranchName))
		} else {
			log.Error(err, "Error getting pull request", "name", key.Name)
			return nil, err
		}
	} else {
		if !utils.IsSameImageRepo(pr.Spec.ImageName, spec.ImageName) {
			err := fmt.Errorf("The image repository is immutable: \"%s\" cannot be changed to \"%s\"", pr.Spec.ImageName, spec.ImageName)
			log.Error(err, "The image repository is immutable", "name", key.Name)
			w.WriteHeader(http.StatusForbidden)
			return nil, err
		}

		patch := client.MergeFrom(pr.DeepCopy())
		pr.Spec = spec

		// Update the PullRequest if it is found
		if err := c.Patch(context.TODO(), &pr, patch); err != nil {
			log.Error(err, "Error updating pull request", "name", key.Name)
			return nil, err
		}

		w.WriteHeader(http.StatusAccepted)
		writeFlush(w, fmt.Sprintf("Updated pull request for branch \"%s\"\n", pr.Spec.BranchName))
	}

	sharedName := utils.GetChildResourceName(reviewApp, &pr)

	attempts := 0
	for {
		bothDone := true

		for _, deploymentSpec := range reviewApp.Spec.Deployments {
			deploymentName := utils.GetResourceName(sharedName, deploymentSpec.Name)

			var deployment = appsv1.Deployment{}
			if err := c.Get(context.TODO(), types.NamespacedName{
				Name:      deploymentName,
				Namespace: key.Namespace,
			}, &deployment); err != nil {
				if apierrors.IsNotFound(err) {
					writeFlush(w, fmt.Sprintf("Waiting for deployment \"%s\" to be created...\n", deploymentName))
					break
				}

				return nil, err
			}

			statusViewer := polymorphichelpers.DeploymentStatusViewer{}

			obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&deployment)
			if err != nil {
				log.Error(err, "Error converting deployment to unstructured")
				return &pr, nil
			}

			status, done, err := statusViewer.Status(&unstructured.Unstructured{Object: obj}, 0)
			if err != nil {
				log.Error(err, "Error getting deployment status")
				return &pr, nil
			}
			if !done {
				bothDone = false
			}

			writeFlush(w, status)
		}

		if bothDone {
			break
		}

		attempts++

		// TODO configuration option
		//
		if attempts > 120 {
			log.Info("Timeout waiting for deployments to be ready")
			http.Error(w, "Timeout waiting for deployments to be ready", http.StatusRequestTimeout)
			break
		}

		time.Sleep(1 * time.Second)
	}

	return &pr, nil
}
