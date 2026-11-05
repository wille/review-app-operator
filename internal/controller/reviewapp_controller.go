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

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// legacyFinalizer was added to ReviewAppConfigs by earlier versions without anything to clean up,
// and blocked deleting them while the operator was not running
const legacyFinalizer = "reviewapp.william.nu/finalizer"

// ReviewAppConfigReconciler reconciles a ReviewAppConfig object
type ReviewAppConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=reviewapps.william.nu,resources=reviewappconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=reviewapps.william.nu,resources=reviewappconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=reviewapps.william.nu,resources=reviewappconfigs/finalizers,verbs=update

// Reconcile removes the finalizer that earlier versions added to ReviewAppConfigs
func (r *ReviewAppConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reviewApp := &reviewapps.ReviewAppConfig{}
	if err := r.Get(ctx, req.NamespacedName, reviewApp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !controllerutil.ContainsFinalizer(reviewApp, legacyFinalizer) {
		return ctrl.Result{}, nil
	}

	// The optimistic lock keeps the finalizers that others add meanwhile
	patch := client.MergeFromWithOptions(reviewApp.DeepCopy(), client.MergeFromWithOptimisticLock{})
	controllerutil.RemoveFinalizer(reviewApp, legacyFinalizer)
	if err := r.Patch(ctx, reviewApp, patch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.FromContext(ctx).Info("Removed the finalizer of earlier versions")
	return ctrl.Result{}, nil
}

func (r *ReviewAppConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&reviewapps.ReviewAppConfig{}).
		Complete(r)
}
