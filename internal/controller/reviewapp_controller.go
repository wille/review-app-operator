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
	"maps"

	// keda "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	racwilliamnuv1alpha1 "github.com/wille/rac/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"k8s.io/apimachinery/pkg/api/equality"
)

const jobOwnerKey = ".metadata.controller"

// ReviewAppReconciler reconciles a ReviewApp object
type ReviewAppReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=rac.william.nu,resources=reviewapps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rac.william.nu,resources=reviewapps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rac.william.nu,resources=reviewapps/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ReviewApp object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.18.2/pkg/reconcile
func (r *ReviewAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	reviewApp := &racwilliamnuv1alpha1.ReviewApp{}
	if err := r.Get(ctx, req.NamespacedName, reviewApp); err != nil {
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	finalizerName := "reviewapp.william.nu/finalizer"

	// examine DeletionTimestamp to determine if object is under deletion
	if reviewApp.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object. This is equivalent
		// to registering our finalizer.
		if !controllerutil.ContainsFinalizer(reviewApp, finalizerName) {
			log.Info("Adding Finalizer for the CronJob", "reviewapp", req.NamespacedName)
			controllerutil.AddFinalizer(reviewApp, finalizerName)
			if err := r.Update(ctx, reviewApp); err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(reviewApp, finalizerName) {
			// our finalizer is present, so lets handle any external dependency
			if err := r.deleteExternalResources(ctx, reviewApp); err != nil {
				// if fail to delete the external dependency here, return with error
				// so that it can be retried.
				return ctrl.Result{}, err
			}

			log.Info("Removed review app", "name", req.Name)

			// remove our finalizer from the list and update it.
			controllerutil.RemoveFinalizer(reviewApp, finalizerName)
			if err := r.Update(ctx, reviewApp); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	// var s keda.ScaledObject
	// log.Info(s)

	// for k, v := range cronJob.Spec.JobTemplate.Annotations {
	// 	job.Annotations[k] = v
	// }
	// job.Annotations[scheduledTimeAnnotation] = scheduledTime.Format(time.RFC3339)
	// for k, v := range cronJob.Spec.JobTemplate.Labels {
	// 	job.Labels[k] = v
	// }

	// for _, childDeployment := range childDeployments.Items {
	// 	if err := r.Delete(ctx, &childDeployment); err != nil {
	// 		log.Error(err, "unable to delete child Deployment", "deployment", childDeployment)
	// 		return ctrl.Result{}, err
	// 	}
	// }

	if err := r.syncDeployments(ctx, &req, reviewApp); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.syncIngresses(ctx, &req, reviewApp); err != nil {
		return ctrl.Result{}, err
	}

	// log.Info("Reconciled ReviewApp")

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ReviewAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &appsv1.Deployment{}, jobOwnerKey, func(rawObj client.Object) []string {
		// grab the job object, extract the owner...
		job := rawObj.(*appsv1.Deployment)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}
		// ...make sure it's a CronJob...
		// TODO check APIVERSION!
		if owner.Kind != "ReviewApp" {
			return nil
		}

		// ...and if so, return it
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &networkingv1.Ingress{}, jobOwnerKey, func(rawObj client.Object) []string {
		// grab the job object, extract the owner...
		job := rawObj.(*networkingv1.Ingress)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}
		// ...make sure it's a CronJob...
		// TODO check APIVERSION!
		if owner.Kind != "ReviewApp" {
			return nil
		}

		// ...and if so, return it
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&racwilliamnuv1alpha1.ReviewApp{}).
		Owns(&appsv1.Deployment{}).
		Owns(&networkingv1.Ingress{}).
		Complete(r)
}

func (r *ReviewAppReconciler) syncIngresses(ctx context.Context, req *ctrl.Request, reviewApp *racwilliamnuv1alpha1.ReviewApp) error {
	log := log.FromContext(ctx)

	var list networkingv1.IngressList
	if err := r.List(ctx, &list, client.InNamespace(req.Namespace), client.MatchingFields{jobOwnerKey: req.Name}); err != nil {
		return err
	}

	if reviewApp.Spec.Ingresses == nil {
		return nil
	}

	// Loop over existing ingresses and update if needed
Test:
	for _, ingressSpec := range reviewApp.Spec.Ingresses {
		ingressName := reviewApp.Name + "-" + ingressSpec.Name

		desiredLabels := getResourceLabels(reviewApp, ingressName, true)

		objectMeta := metav1.ObjectMeta{
			Labels:      desiredLabels,
			Annotations: reviewApp.Annotations,
			Name:        ingressName,
			Namespace:   reviewApp.Namespace,
		}

		desiredIngress := &networkingv1.Ingress{
			ObjectMeta: objectMeta,
			Spec:       ingressSpec.Spec,
		}

		for _, activeIngress := range list.Items {
			if activeIngress.Name == ingressName {
				patch := client.MergeFrom(activeIngress.DeepCopy())

				// Deployment labels or spec differs from desired spec
				if !equality.Semantic.DeepDerivative(desiredIngress.Spec, activeIngress.Spec) {
					activeIngress.Spec = desiredIngress.Spec

					if err := r.Patch(ctx, &activeIngress, patch); err != nil {
						return err
					}

					log.Info("ingress updated", "ingressName", ingressName)
				}

				continue Test
			}
		}

		log.Info("Creating ingress", "ingressName", ingressName)

		if err := ctrl.SetControllerReference(reviewApp, desiredIngress, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, desiredIngress); err != nil {
			log.Error(err, "unable to create ingress", "ingress", desiredIngress)
			return err
		}
	}

	return nil
}

// syncDeployments syncs all active deployments defined in the ReviewApp spec
func (r *ReviewAppReconciler) syncDeployments(ctx context.Context, req *ctrl.Request, reviewApp *racwilliamnuv1alpha1.ReviewApp) error {
	log := log.FromContext(ctx)

	var list appsv1.DeploymentList
	if err := r.List(ctx, &list, client.InNamespace(req.Namespace), client.MatchingFields{jobOwnerKey: req.Name}); err != nil {
		return err
	}

	if reviewApp.Spec.Deployments == nil {
		return nil
	}

	// Loop over existing deployments and update if needed

Test:
	for _, deploymentSpec := range reviewApp.Spec.Deployments {
		deploymentName := reviewApp.Name + "-" + deploymentSpec.Name

		desiredLabels := getResourceLabels(reviewApp, deploymentName, true)

		// PodSpec.Selector is immutable, so we need to recreate the Deployment if labels change
		selectorLabels := getResourceLabels(reviewApp, deploymentName, false)

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

		for _, runningDeployment := range list.Items {
			if runningDeployment.Name == deploymentName {
				patch := client.MergeFrom(runningDeployment.DeepCopy())

				// Detect if desired labels have changed
				if !equality.Semantic.DeepEqual(desiredDeployment.ObjectMeta.Labels, runningDeployment.ObjectMeta.Labels) {
					if err := r.Delete(ctx, &runningDeployment); err != nil {
						return err
					}

					log.Info("Labels changed, recreating Deployment since PodSpec.Selector is immutable")
					break Test
				}

				// Deployment labels or spec differs from desired spec
				if !equality.Semantic.DeepDerivative(desiredDeployment.Spec, runningDeployment.Spec) {
					runningDeployment.Spec = desiredDeployment.Spec

					if err := r.Patch(ctx, &runningDeployment, patch); err != nil {
						return err
					}

					log.Info("Deployment updated", "deploymentName", deploymentName)
				}

				continue Test
			}
		}

		log.Info("Creating deployment", "deploymentName", deploymentName)

		if err := ctrl.SetControllerReference(reviewApp, desiredDeployment, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, desiredDeployment); err != nil {
			log.Error(err, "unable to create deployment", "deployment", desiredDeployment)
			return err
		}
	}

	return nil
}

// getResourceLabels returns the labels for all ReviewApp child resources
func getResourceLabels(reviewApp *racwilliamnuv1alpha1.ReviewApp, instance string, includeUserLabels bool) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/name":       reviewApp.Name,
		"app.kubernetes.io/instance":   instance,
		"app.kubernetes.io/version":    "Target",
		"app.kubernetes.io/component":  "review-app",
		"app.kubernetes.io/part-of":    reviewApp.Name,
		"app.kubernetes.io/managed-by": "review-app-controller",
	}

	if includeUserLabels {
		maps.Copy(labels, reviewApp.ObjectMeta.Labels)
	}

	return labels
}

func (r *ReviewAppReconciler) deleteExternalResources(ctx context.Context, cronJob *racwilliamnuv1alpha1.ReviewApp) error {
	//

	// log := log.FromContext(ctx)
	// // delete any external resources associated with the cronJob
	// //
	// // Ensure that delete implementation is idempotent and safe to invoke
	// // multiple times for same object.

	// childJobs := appsv1.DeploymentList{}
	// if err := r.List(ctx, &childJobs, client.MatchingFields{".metadata.controller": req.Name}); err != nil {
	// 	log.Error(err, "unable to list child Jobs")
	// 	return err
	// }

	return nil
}
