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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/intstr"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"github.com/wille/review-app-operator/internal/utils"
)

const (
	reviewAppRefField   = ".spec.reviewAppRef"
	pullRequestOwnerKey = ".metadata.controller"
)

// PullRequestReconciler reconciles a PullRequest object
type PullRequestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=reviewapps.william.nu,resources=pullrequests,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=reviewapps.william.nu,resources=pullrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=reviewapps.william.nu,resources=pullrequests/finalizers,verbs=update

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get

// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services/status,verbs=get

func (r *PullRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	pr := &reviewapps.PullRequest{}
	if err := r.Get(ctx, req.NamespacedName, pr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The reviewApp referenced by the PullRequest
	var reviewApp reviewapps.ReviewApp
	if err := r.Get(ctx, types.NamespacedName{Name: pr.Spec.ReviewAppRef, Namespace: req.Namespace}, &reviewApp); err != nil {
		if apierrors.IsNotFound(err) {
			// No ReviewApp for reviewAppRef found
			log.Error(nil, "ReviewApp not found", "reviewAppRef", pr.Spec.ReviewAppRef)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// To handle PullRequest resources manually created and to not need to take ownership at creation time
	// we take ownership here if it's missing
	if !controllerutil.HasControllerReference(pr) {
		log.Info("Taking ownership of", "PullRequest", pr.Name)
		if err := ctrl.SetControllerReference(&reviewApp, pr, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Update(ctx, pr); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Shared name for all child resources
	sharedName := utils.GetChildResourceName(&reviewApp, pr)

	// Deployments owned by this PullRequest
	var list appsv1.DeploymentList
	if err := r.List(ctx, &list, client.InNamespace(req.Namespace), client.MatchingFields{pullRequestOwnerKey: req.Name}); err != nil {
		return ctrl.Result{}, err
	}

	// Delete deployments that are not in the spec
	for _, runningDeployment := range list.Items {
		found := false

		for _, deploymentSpec := range reviewApp.Spec.Deployments {
			deploymentName := utils.GetResourceName(sharedName, deploymentSpec.Name)
			selectorLabels := utils.GetSelectorLabels(&reviewApp, *pr, deploymentSpec.Name)

			// If the deployment selector labels does not match, then we need to recreate the deployment as the selector labels are immutable
			// This should not happen as the selector labels are derived from the PullRequest and ReviewApp but guard anyways against having stale deployments
			if runningDeployment.ObjectMeta.Name == deploymentName &&
				equality.Semantic.DeepDerivative(selectorLabels, runningDeployment.ObjectMeta.Labels) {
				found = true
				break
			}
		}

		if !found {
			log.Info("Deleting deployment", "name", runningDeployment.Name)
			if err := r.Delete(ctx, &runningDeployment); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Loop all desired deployments in the ReviewApp spec
	for _, deploymentSpec := range reviewApp.Spec.Deployments {
		deploymentName := utils.GetResourceName(sharedName, deploymentSpec.Name)

		// Desired labels for all subresources, including all labels set on the ReviewApp
		desiredLabels := utils.GetResourceLabels(&reviewApp, *pr, deploymentSpec.Name)

		// PodSpec.Selector is immutable, so we need to recreate the Deployment if labels change
		// so selectorLabels does not include user labels
		selectorLabels := utils.GetSelectorLabels(&reviewApp, *pr, deploymentSpec.Name)

		objectMeta := metav1.ObjectMeta{
			Name:        deploymentName,
			Labels:      desiredLabels,
			Namespace:   reviewApp.Namespace,
			Annotations: make(map[string]string),
		}

		// Merge pod template labels with the ReviewApp labels
		podTemplate := deploymentSpec.Template.DeepCopy()
		if podTemplate.ObjectMeta.Labels == nil {
			podTemplate.ObjectMeta.Labels = make(map[string]string)
		}
		maps.Copy(podTemplate.ObjectMeta.Labels, objectMeta.Labels)

		desiredDeployment := &appsv1.Deployment{
			ObjectMeta: *objectMeta.DeepCopy(),
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: selectorLabels,
				},
				Template: *podTemplate,
			},
		}

		// Get the hostnames for this deployment
		hostnames, err := utils.GetHostnamesFromTemplate(deploymentSpec.HostTemplates, deploymentSpec.Name, *pr, reviewApp)
		if err != nil {
			return ctrl.Result{}, err
		}
		hostnameAnnotationValue, err := json.Marshal(hostnames)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Annotate the deployment with the hostnames for this pull request.
		// The forwarder will target this deployment and keep track of requests
		desiredDeployment.ObjectMeta.Annotations[utils.HostAnnotation] = string(hostnameAnnotationValue)

		// Update the target container image with the latest PR image if any is set on the PullRequest
		if pr.Spec.ImageName != "" {
			for i := 0; i < len(desiredDeployment.Spec.Template.Spec.Containers); i++ {
				container := &desiredDeployment.Spec.Template.Spec.Containers[i]
				if deploymentSpec.TargetContainerName == container.Name {
					container.Image = pr.Spec.ImageName
					break
				}
			}
		}

		var runningDeployment appsv1.Deployment
		if err := r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: reviewApp.Namespace}, &runningDeployment); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}

			if err := ctrl.SetControllerReference(pr, desiredDeployment, r.Scheme); err != nil {
				return ctrl.Result{}, err
			}

			// New deployments are downscaled by default.
			// The problem with this approach is that the incoming deploy webhook
			// will not be able to tell if the pod actually started and became healthy
			var replicas int32 = 0
			desiredDeployment.Spec.Replicas = &replicas

			// Set the "last request" time so the downscaler can process it
			desiredDeployment.ObjectMeta.Annotations[utils.LastRequestTimeAnnotation] = time.Now().Format(time.RFC3339)

			if err := r.Create(ctx, desiredDeployment); err != nil {
				return ctrl.Result{}, err
			}

			log.Info("Deployment created", "deploymentName", deploymentName)
		} else {
			// If the deployment spec in the ReviewApp gets updated we do not update the deployment
			// The deployment has to be manually deleted.
			// TODO patch image change
			if !equality.Semantic.DeepDerivative(desiredDeployment.Spec.Template, runningDeployment.Spec.Template) ||
				!equality.Semantic.DeepDerivative(desiredDeployment.ObjectMeta.Labels, runningDeployment.ObjectMeta.Labels) ||
				runningDeployment.ObjectMeta.Annotations[utils.HostAnnotation] !=
					desiredDeployment.ObjectMeta.Annotations[utils.HostAnnotation] {

				patch := client.MergeFrom(runningDeployment.DeepCopy())

				runningDeployment.ObjectMeta.Labels = desiredLabels
				runningDeployment.Spec.Template = desiredDeployment.Spec.Template
				runningDeployment.ObjectMeta.Annotations[utils.HostAnnotation] = string(hostnameAnnotationValue)

				if err := r.Patch(ctx, &runningDeployment, patch); err != nil {
					return ctrl.Result{}, err
				}

				log.Info("Deployment updated", "deploymentName", deploymentName)
			}
		}

		desiredSvc := &corev1.Service{
			ObjectMeta: *objectMeta.DeepCopy(),
			Spec: corev1.ServiceSpec{
				Selector: selectorLabels,
				Type:     corev1.ServiceTypeClusterIP,
				Ports: []corev1.ServicePort{
					{
						Name: "http",

						// Always use port 80, the forwarder assumes this as well
						Port:       80,
						TargetPort: intstr.FromInt(int(deploymentSpec.TargetContainerPort)),
					},
				},
			},
		}

		// If a .spec.deployment.name changes, the old service will not be cleaned up
		var activeSvc corev1.Service
		if err := r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: reviewApp.Namespace}, &activeSvc); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}

			if err := ctrl.SetControllerReference(pr, desiredSvc, r.Scheme); err != nil {
				return ctrl.Result{}, err
			}

			if err := r.Create(ctx, desiredSvc); err != nil {
				return ctrl.Result{}, err
			}

			log.Info("Service created", "serviceName", deploymentName)
		} else {
			// Service labels or spec differs from desired spec
			if !equality.Semantic.DeepDerivative(desiredSvc.Spec, activeSvc.Spec) ||
				!equality.Semantic.DeepDerivative(desiredSvc.ObjectMeta.Labels, activeSvc.ObjectMeta.Labels) {
				patch := client.MergeFrom(activeSvc.DeepCopy())

				activeSvc.ObjectMeta.Labels = desiredSvc.ObjectMeta.Labels
				activeSvc.Spec = desiredSvc.Spec

				if err := r.Patch(ctx, &activeSvc, patch); err != nil {
					return ctrl.Result{}, err
				}

				log.Info("Service updated", "serviceName", deploymentName)
			}
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PullRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &appsv1.Deployment{}, pullRequestOwnerKey, func(rawObj client.Object) []string {
		job := rawObj.(*appsv1.Deployment)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}

		// TODO Do we need to check the APIVersion?
		if owner.Kind != "PullRequest" {
			return nil
		}

		// Index by the Pull Request name
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Service{}, pullRequestOwnerKey, func(rawObj client.Object) []string {
		job := rawObj.(*corev1.Service)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}

		// TODO Do we need to check the APIVersion?
		if owner.Kind != "PullRequest" {
			return nil
		}

		// Index by the Pull Request name
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	// Index PullRequest resources based on .spec.reviewAppRef
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &reviewapps.PullRequest{}, reviewAppRefField, func(rawObj client.Object) []string {
		configDeployment := rawObj.(*reviewapps.PullRequest)
		if configDeployment.Spec.ReviewAppRef == "" {
			return nil
		}
		return []string{configDeployment.Spec.ReviewAppRef}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&reviewapps.PullRequest{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		// Watch for changes to ReviewApp resources
		Watches(
			&reviewapps.ReviewApp{},
			handler.EnqueueRequestsFromMapFunc(r.findPullRequestsForReviewApp),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Complete(r)
}

func SetupHostIndex(mgr ctrl.Manager) error {
	// Index deployments by the hosts annotation to query them in the forwarder
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &appsv1.Deployment{}, utils.HostIndexFieldName, func(rawObj client.Object) []string {
		svc := rawObj.(*appsv1.Deployment)

		if enc, ok := svc.Annotations[utils.HostAnnotation]; ok {
			var hosts []string
			if err := json.Unmarshal([]byte(enc), &hosts); err != nil {
				fmt.Printf("Failed to unmarshal deployment %s host annotation: %s\n", svc.Name, err)
				return []string{}
			}
			return hosts
		}

		return []string{}
	}); err != nil {
		return err
	}

	return nil
}

// findPullRequestsForReviewApp returns a list of PullRequest resources that reference the given ReviewApp
func (r *PullRequestReconciler) findPullRequestsForReviewApp(ctx context.Context, reviewApp client.Object) []reconcile.Request {
	var prs reviewapps.PullRequestList
	err := r.List(ctx, &prs, &client.ListOptions{
		FieldSelector: fields.OneTermEqualSelector(reviewAppRefField, reviewApp.GetName()),
		Namespace:     reviewApp.GetNamespace(),
	})
	if err != nil {
		return []reconcile.Request{}
	}

	requests := make([]reconcile.Request, len(prs.Items))
	for i, item := range prs.Items {
		requests[i] = reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      item.GetName(),
				Namespace: item.GetNamespace(),
			},
		}
	}
	return requests
}
