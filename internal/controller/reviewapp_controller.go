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

	// keda "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// ReviewAppConfigReconciler reconciles a ReviewAppConfig object
type ReviewAppConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=reviewapps.william.nu,resources=reviewconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=reviewapps.william.nu,resources=reviewconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=reviewapps.william.nu,resources=reviewconfigs/finalizers,verbs=update

func (r *ReviewAppConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	reviewApp := &reviewapps.ReviewAppConfig{}
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
			log.Info("Adding Finalizer for the ReviewAppConfig", "reviewapp", req.NamespacedName)
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
	var list reviewapps.PullRequestList
	if err := r.List(ctx, &list, client.InNamespace(req.Namespace), client.MatchingFields{"spec.reviewAppRef": req.Name}); err != nil {
		log.Error(err, "unable to list child Jobs")
		return ctrl.Result{}, err
	}

	// if err := r.Status().Update(ctx, reviewApp); err != nil {
	// 	return ctrl.Result{}, err
	// }

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ReviewAppConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &reviewapps.PullRequest{}, "spec.reviewAppRef", func(rawObj client.Object) []string {
		// grab the job object, extract the owner...
		job := rawObj.(*reviewapps.PullRequest)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}
		// ...make sure it's a CronJob...
		// TODO check APIVERSION!
		if owner.Kind != "ReviewAppConfig" {
			return nil
		}

		// ...and if so, return it
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&reviewapps.ReviewAppConfig{}).
		Owns(&reviewapps.PullRequest{}).
		Complete(r)
}

func (r *ReviewAppConfigReconciler) deleteExternalResources(ctx context.Context, cronJob *reviewapps.ReviewAppConfig) error {
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
