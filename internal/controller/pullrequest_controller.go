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
	"errors"
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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
	var reviewApp reviewapps.ReviewAppConfig
	if err := r.Get(ctx, types.NamespacedName{Name: pr.Spec.ReviewAppConfigRef, Namespace: req.Namespace}, &reviewApp); err != nil {
		if apierrors.IsNotFound(err) {
			// No ReviewAppConfig for reviewAppRef found. The PullRequest is reconciled again when it is created
			log.Error(nil, "ReviewAppConfig not found", "reviewAppRef", pr.Spec.ReviewAppConfigRef)
			patch := client.MergeFrom(pr.DeepCopy())
			setReconciledCondition(pr, fmt.Errorf("%w: %s", errReviewAppConfigNotFound, pr.Spec.ReviewAppConfigRef))
			return ctrl.Result{}, r.Status().Patch(ctx, pr, patch)
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
		// return ctrl.Result{ }, nil
	}

	// Deployments owned by this PullRequest
	var deploymentList appsv1.DeploymentList
	if err := r.List(ctx, &deploymentList, client.InNamespace(req.Namespace), client.MatchingFields{pullRequestOwnerKey: req.Name}); err != nil {
		return ctrl.Result{}, err
	}

	// Delete deployments that are not in the spec
	for _, runningDeployment := range deploymentList.Items {
		found := false

		for _, deploymentSpec := range reviewApp.Spec.Deployments {
			deploymentName := utils.GetDeploymentName(&reviewApp, pr, deploymentSpec.Name)
			selectorLabels := utils.GetSelectorLabels(&reviewApp, *pr, deploymentSpec.Name)

			// If the deployment name or selector labels does not match, then we need to recreate the deployment as the selector labels are immutable
			// This should not happen as the selector labels are derived from the PullRequest and ReviewAppConfig but guard anyways against having stale deployments
			if runningDeployment.Name == deploymentName &&
				equality.Semantic.DeepDerivative(selectorLabels, runningDeployment.Labels) {
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

	existingPr := pr.DeepCopy()
	patch := client.MergeFrom(existingPr)

	if pr.Status.Deployments == nil {
		pr.Status.Deployments = make(map[string]*reviewapps.DeploymentStatus)
	}

	// Loop all desired deployments in the ReviewAppConfig spec
	var reconcileErr error
	for _, deploymentSpec := range reviewApp.Spec.Deployments {
		if reconcileErr = r.reconcileDeployment(ctx, &reviewApp, pr, deploymentSpec); reconcileErr != nil {
			break
		}
	}

	// Record the error on the PullRequest, where the webhook reports it to CI
	setReconciledCondition(pr, reconcileErr)

	if !equality.Semantic.DeepEqual(existingPr.Status, pr.Status) {
		if err := r.Status().Patch(ctx, pr, patch); err != nil {
			return ctrl.Result{}, errors.Join(reconcileErr, err)
		}
	}

	return ctrl.Result{}, reconcileErr
}

// Reasons of the Reconciled condition
const (
	reasonReconciled              = "Reconciled"
	reasonReviewAppConfigNotFound = "ReviewAppConfigNotFound"
	reasonInvalidHostTemplate     = "InvalidHostTemplate"
	reasonNameConflict            = "NameConflict"
	reasonReconcileFailed         = "ReconcileFailed"
)

var (
	errReviewAppConfigNotFound = errors.New("ReviewAppConfig not found")

	// errNotControlled is returned when a deployment or service of the review app belongs to
	// something else, as different branch names can be truncated to the same names
	errNotControlled = errors.New("is not controlled by PullRequest")
)

// setReconciledCondition records on the PullRequest status whether it could be reconciled
func setReconciledCondition(pr *reviewapps.PullRequest, err error) {
	condition := metav1.Condition{
		Type:               reviewapps.ConditionReconciled,
		Status:             metav1.ConditionTrue,
		Reason:             reasonReconciled,
		ObservedGeneration: pr.Generation,
	}

	if err != nil {
		condition.Status = metav1.ConditionFalse
		condition.Message = err.Error()

		switch {
		case errors.Is(err, errReviewAppConfigNotFound):
			condition.Reason = reasonReviewAppConfigNotFound
		case errors.Is(err, utils.ErrInvalidHostTemplate):
			condition.Reason = reasonInvalidHostTemplate
		case errors.Is(err, errNotControlled):
			condition.Reason = reasonNameConflict
		default:
			condition.Reason = reasonReconcileFailed
		}
	}

	// The deployment statuses are required by the CRD
	if pr.Status.Deployments == nil {
		pr.Status.Deployments = make(map[string]*reviewapps.DeploymentStatus)
	}

	meta.SetStatusCondition(&pr.Status.Conditions, condition)
}

// reconcileDeployment ensures the Deployment and its Service for a single
// ReviewAppConfig deployment spec exist and match the desired state, and
// records the deployment's hostnames on the PullRequest status.
func (r *PullRequestReconciler) reconcileDeployment(
	ctx context.Context,
	reviewApp *reviewapps.ReviewAppConfig,
	pr *reviewapps.PullRequest,
	deploymentSpec reviewapps.Deployments,
) error {
	log := log.FromContext(ctx)

	deploymentName := utils.GetDeploymentName(reviewApp, pr, deploymentSpec.Name)

	// Desired labels for all subresources, including all labels set on the ReviewAppConfig
	desiredLabels := utils.GetResourceLabels(reviewApp, *pr, deploymentSpec.Name)

	// PodSpec.Selector is immutable, so we need to recreate the Deployment if labels change
	// so selectorLabels does not include user labels
	selectorLabels := utils.GetSelectorLabels(reviewApp, *pr, deploymentSpec.Name)

	objectMeta := metav1.ObjectMeta{
		Name:        deploymentName,
		Labels:      desiredLabels,
		Namespace:   reviewApp.Namespace,
		Annotations: make(map[string]string),
	}

	// Merge pod template labels with the ReviewAppConfig labels
	podTemplate := deploymentSpec.Template.DeepCopy()
	if podTemplate.Labels == nil {
		podTemplate.Labels = make(map[string]string)
	}
	maps.Copy(podTemplate.Labels, objectMeta.Labels)

	desiredDeployment := &appsv1.Deployment{
		ObjectMeta: *objectMeta.DeepCopy(),
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			Replicas:                &deploymentSpec.Replicas,
			Template:                *podTemplate,
			Strategy:                deploymentSpec.Strategy,
			MinReadySeconds:         deploymentSpec.MinReadySeconds,
			ProgressDeadlineSeconds: deploymentSpec.ProgressDeadlineSeconds,
		},
	}

	// Get the hostnames for this deployment
	hostnames, err := utils.GetHostnamesFromTemplate(deploymentSpec.HostTemplates, deploymentSpec.Name, *pr, *reviewApp)
	if err != nil {
		return err
	}

	if pr.Status.Deployments[deploymentSpec.Name] == nil {
		pr.Status.Deployments[deploymentSpec.Name] = &reviewapps.DeploymentStatus{
			LastActive: metav1.Now(),
			IsActive:   deploymentSpec.StartOnDeploy || reviewApp.Spec.StartOnDeploy,
		}
	}

	pr.Status.Deployments[deploymentSpec.Name].Hostnames = hostnames

	var runningDeployment appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: reviewApp.Namespace}, &runningDeployment); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}

		if err := ctrl.SetControllerReference(pr, desiredDeployment, r.Scheme); err != nil {
			return err
		}

		if pr.Spec.ImageName != "" {
			updateContainerImage(desiredDeployment, deploymentSpec.TargetContainerName, pr.Spec.ImageName)
		}

		// Only start the deployment if the pull request is active
		replicas := int32(0)
		if pr.Status.Deployments[deploymentSpec.Name].IsActive {
			replicas = max(deploymentSpec.Replicas, 1)
		}
		desiredDeployment.Spec.Replicas = &replicas

		if err := r.Create(ctx, desiredDeployment); err != nil {
			return err
		}

		log.Info("Deployment created", "deploymentName", deploymentName)
	} else {
		// Different branch names can be truncated to the same deployment name
		if !metav1.IsControlledBy(&runningDeployment, pr) {
			return fmt.Errorf("deployment %s %w %s", deploymentName, errNotControlled, pr.Name)
		}

		patch := client.MergeFrom(runningDeployment.DeepCopy())

		// Update the target container image with the latest PR image if any is set on the PullRequest
		updatedImage := false
		if pr.Spec.ImageName != "" {
			updatedImage = updateContainerImage(&runningDeployment, deploymentSpec.TargetContainerName, pr.Spec.ImageName)
		}

		if updatedImage {
			// Changes to the deployment spec in the ReviewAppConfig roll out with the next image
			// pushed to each pull request, which restarts the pods anyway, instead of restarting
			// every review app at once when the ReviewAppConfig changes
			runningDeployment.Spec.Template = *desiredDeployment.Spec.Template.DeepCopy()
			runningDeployment.Spec.Strategy = desiredDeployment.Spec.Strategy
			runningDeployment.Spec.MinReadySeconds = desiredDeployment.Spec.MinReadySeconds
			runningDeployment.Spec.ProgressDeadlineSeconds = desiredDeployment.Spec.ProgressDeadlineSeconds
			updateContainerImage(&runningDeployment, deploymentSpec.TargetContainerName, pr.Spec.ImageName)

			log.Info("Updated image", "deploymentName", deploymentName, "image", pr.Spec.ImageName)
		}

		replicas := *runningDeployment.Spec.Replicas
		isActive := pr.Status.Deployments[deploymentSpec.Name].IsActive
		if replicas == 0 {
			// Only scale up if deployment is active and the user has not manually scaled the deployment
			if isActive {
				if deploymentSpec.Replicas != 0 {
					replicas = deploymentSpec.Replicas
				} else {
					replicas = 1
				}
			}
		} else if !isActive {
			// Scale down if deployment is not active
			replicas = 0
		}

		if updatedImage ||
			*runningDeployment.Spec.Replicas != replicas ||
			!equality.Semantic.DeepDerivative(desiredDeployment.Labels, runningDeployment.Labels) {

			runningDeployment.Spec.Replicas = &replicas
			runningDeployment.Labels = desiredLabels

			if err := r.Patch(ctx, &runningDeployment, patch); err != nil {
				return err
			}

			log.Info("Deployment updated", "deploymentName", deploymentName)
		}
	}

	return r.reconcileService(ctx, reviewApp, pr, deploymentSpec, objectMeta, selectorLabels)
}

// reconcileService ensures the Service for a single deployment spec exists and
// matches the desired state.
func (r *PullRequestReconciler) reconcileService(
	ctx context.Context,
	reviewApp *reviewapps.ReviewAppConfig,
	pr *reviewapps.PullRequest,
	deploymentSpec reviewapps.Deployments,
	objectMeta metav1.ObjectMeta,
	selectorLabels map[string]string,
) error {
	log := log.FromContext(ctx)

	deploymentName := utils.GetDeploymentName(reviewApp, pr, deploymentSpec.Name)

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
			return err
		}

		if err := ctrl.SetControllerReference(pr, desiredSvc, r.Scheme); err != nil {
			return err
		}

		if err := r.Create(ctx, desiredSvc); err != nil {
			return err
		}

		log.Info("Service created", "serviceName", deploymentName)
		return nil
	}

	// Different branch names can be truncated to the same service name
	if !metav1.IsControlledBy(&activeSvc, pr) {
		return fmt.Errorf("service %s %w %s", deploymentName, errNotControlled, pr.Name)
	}

	// Service labels or spec differs from desired spec
	if !equality.Semantic.DeepDerivative(desiredSvc.Spec, activeSvc.Spec) ||
		!equality.Semantic.DeepDerivative(desiredSvc.Labels, activeSvc.Labels) {
		patch := client.MergeFrom(activeSvc.DeepCopy())

		activeSvc.Labels = desiredSvc.Labels
		activeSvc.Spec = desiredSvc.Spec

		if err := r.Patch(ctx, &activeSvc, patch); err != nil {
			return err
		}

		log.Info("Service updated", "serviceName", deploymentName)
	}

	return nil
}

func updateContainerImage(deploy *appsv1.Deployment, containerName, image string) bool {
	for i := 0; i < len(deploy.Spec.Template.Spec.Containers); i++ {
		container := &deploy.Spec.Template.Spec.Containers[i]
		if container.Name == containerName {
			isUpdated := container.Image != image

			container.Image = image

			return isUpdated
		}
	}

	return false
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
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &reviewapps.PullRequest{}, utils.ReviewAppRefField, func(rawObj client.Object) []string {
		configDeployment := rawObj.(*reviewapps.PullRequest)
		if configDeployment.Spec.ReviewAppConfigRef == "" {
			return nil
		}
		return []string{configDeployment.Spec.ReviewAppConfigRef}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&reviewapps.PullRequest{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		// Watch for changes to ReviewAppConfig resources
		Watches(
			&reviewapps.ReviewAppConfig{},
			handler.EnqueueRequestsFromMapFunc(r.findPullRequestsForReviewAppConfig),
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Complete(r)
}

func SetupHostIndex(mgr ctrl.Manager) error {
	// Index deployments by the hosts annotation to query them in the forwarder
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &reviewapps.PullRequest{}, utils.HostIndexFieldName, func(rawObj client.Object) []string {
		pr := rawObj.(*reviewapps.PullRequest)

		hosts := make([]string, 0, len(pr.Status.Deployments))

		for _, d := range pr.Status.Deployments {
			hosts = append(hosts, d.Hostnames...)
		}

		return hosts
	}); err != nil {
		return err
	}

	return nil
}

// findPullRequestsForReviewAppConfig returns a list of PullRequest resources that reference the given ReviewAppConfig
func (r *PullRequestReconciler) findPullRequestsForReviewAppConfig(ctx context.Context, reviewApp client.Object) []reconcile.Request {
	var prs reviewapps.PullRequestList
	err := r.List(ctx, &prs, &client.ListOptions{
		FieldSelector: fields.OneTermEqualSelector(utils.ReviewAppRefField, reviewApp.GetName()),
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
