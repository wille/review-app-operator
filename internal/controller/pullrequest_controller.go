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

	racwilliamnuv1alpha1 "github.com/wille/rac/api/v1alpha1"
	"github.com/wille/rac/internal/utils"
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

// +kubebuilder:rbac:groups=rac.william.nu,resources=pullrequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rac.william.nu,resources=pullrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rac.william.nu,resources=pullrequests/finalizers,verbs=update

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get
// +kubebuilder:rbac:groups=rac.william.nu,resources=reviewapps,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the PullRequest object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.18.2/pkg/reconcile
func (r *PullRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	pr := &racwilliamnuv1alpha1.PullRequest{}
	if err := r.Get(ctx, req.NamespacedName, pr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// The reviewApp referenced by the PullRequest
	var reviewApp racwilliamnuv1alpha1.ReviewApp
	if err := r.Get(ctx, types.NamespacedName{Name: pr.Spec.ReviewAppRef, Namespace: req.Namespace}, &reviewApp); err != nil {
		if apierrors.IsNotFound(err) {
			// No ReviewApp for reviewAppRef found
			log.Info("ReviewApp not found", "reviewAppRef", pr.Spec.ReviewAppRef)
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

	// Deployments owned by this PullRequest
	var list appsv1.DeploymentList
	if err := r.List(ctx, &list, client.InNamespace(req.Namespace), client.MatchingFields{pullRequestOwnerKey: req.Name}); err != nil {
		return ctrl.Result{}, err
	}

	// Delete deployments that are not in the spec
	for _, runningDeployment := range list.Items {
		found := false

		for _, deploymentSpec := range reviewApp.Spec.Deployments {
			name := utils.GetResourceName(utils.GetResourceNameFrom(&reviewApp, pr), deploymentSpec.Name)
			if runningDeployment.ObjectMeta.Name == name {
				found = true
				break
			}
		}

		if !found {
			log.Info("Deleting deployment not in spec", "name", runningDeployment.ObjectMeta.Name)
			if err := r.Delete(ctx, &runningDeployment); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Loop all desired deployments in the ReviewApp spec
	for _, deploymentSpec := range reviewApp.Spec.Deployments {
		deploymentName := utils.GetResourceName(utils.GetResourceNameFrom(&reviewApp, pr), deploymentSpec.Name)

		desiredLabels := getResourceLabels(&reviewApp, deploymentName, true)

		// PodSpec.Selector is immutable, so we need to recreate the Deployment if labels change
		selectorLabels := getResourceLabels(&reviewApp, deploymentName, false)

		objectMeta := metav1.ObjectMeta{
			Labels:      desiredLabels,
			Annotations: reviewApp.Annotations,
			Name:        deploymentName,
			Namespace:   reviewApp.Namespace,
		}

		replicas := int32(1)

		desiredDeployment := &appsv1.Deployment{
			ObjectMeta: objectMeta,
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{
					MatchLabels: selectorLabels,
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: objectMeta,
					Spec:       deploymentSpec.Spec,
				},
			},
		}

		createNew := true

		for _, runningDeployment := range list.Items {
			if runningDeployment.ObjectMeta.Name != deploymentName {
				continue
			}
			patch := client.MergeFrom(runningDeployment.DeepCopy())

			// Deployment labels or spec differs from desired spec
			if !equality.Semantic.DeepDerivative(desiredDeployment.Spec, runningDeployment.Spec) ||
				!equality.Semantic.DeepEqual(desiredLabels, runningDeployment.ObjectMeta.Labels) {
				runningDeployment.ObjectMeta.Labels = desiredLabels
				runningDeployment.Spec = desiredDeployment.Spec

				if err := r.Patch(ctx, &runningDeployment, patch); err != nil {
					return ctrl.Result{}, err
				}

				log.Info("Deployment updated", "deploymentName", deploymentName)
			}
			createNew = false
			break
		}

		if createNew {
			if err := ctrl.SetControllerReference(pr, desiredDeployment, r.Scheme); err != nil {
				return ctrl.Result{}, err
			}

			if err := r.Create(ctx, desiredDeployment); err != nil {
				return ctrl.Result{}, err
			}

			log.Info("Deployment created", "deploymentName", deploymentName)
		}

		// Add all ports from all containers to the service
		ports := []corev1.ServicePort{}
		for _, container := range deploymentSpec.Spec.Containers {
			for _, port := range container.Ports {
				// TODO Name must be set on a svc if there is more than 1 port
				ports = append(ports, corev1.ServicePort{
					Name:       port.Name,
					Port:       port.ContainerPort,
					TargetPort: intstr.FromInt32(port.ContainerPort),
				})
			}
		}

		desiredSvc := &corev1.Service{
			ObjectMeta: objectMeta,
			Spec: corev1.ServiceSpec{
				Selector: selectorLabels,
				Type:     corev1.ServiceTypeClusterIP,
				Ports:    ports,
			},
		}
		var activeSvc corev1.Service
		if err := r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: reviewApp.Namespace}, &activeSvc); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}

			// Service for deployment not found, create it and take ownership
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
				!equality.Semantic.DeepEqual(desiredLabels, activeSvc.ObjectMeta.Labels) {
				patch := client.MergeFrom(activeSvc.DeepCopy())

				activeSvc.ObjectMeta.Labels = desiredLabels
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
		// grab the job object, extract the owner...
		job := rawObj.(*appsv1.Deployment)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}

		// TODO check APIVERSION!
		if owner.Kind != "PullRequest" {
			return nil
		}

		// ...and if so, return it
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	// Index PullRequest resources based on .spec.reviewAppRef
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &racwilliamnuv1alpha1.PullRequest{}, reviewAppRefField, func(rawObj client.Object) []string {
		configDeployment := rawObj.(*racwilliamnuv1alpha1.PullRequest)
		if configDeployment.Spec.ReviewAppRef == "" {
			return nil
		}
		return []string{configDeployment.Spec.ReviewAppRef}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&racwilliamnuv1alpha1.PullRequest{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		// Watch for changes to ReviewApp resources
		Watches(
			&racwilliamnuv1alpha1.ReviewApp{},
			handler.EnqueueRequestsFromMapFunc(r.findPullRequestsForReviewApp),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Complete(r)
}

// findPullRequestsForReviewApp returns a list of PullRequest resources that reference the given ReviewApp
func (r *PullRequestReconciler) findPullRequestsForReviewApp(ctx context.Context, reviewApp client.Object) []reconcile.Request {
	var prs racwilliamnuv1alpha1.PullRequestList
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
