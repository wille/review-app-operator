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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
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

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ReviewAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &racwilliamnuv1alpha1.PullRequest{}, "spec.reviewAppRef", func(rawObj client.Object) []string {
		// grab the job object, extract the owner...
		job := rawObj.(*racwilliamnuv1alpha1.PullRequest)
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
		Owns(&racwilliamnuv1alpha1.PullRequest{}).
		Complete(r)
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
