package controller

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
)

// The activity of a review app deployment is recorded in a Lease named like the Deployment. The
// forwarder renews it when the deployment is visited, and the deployment runs while the Lease is
// held: until it was last renewed plus scaleDownAfter, which is the duration of the Lease.

// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// scaleDownAfter is how long a deployment runs after it was last active, or 0 to keep it running
func scaleDownAfter(reviewApp *reviewapps.ReviewAppConfig, spec reviewapps.Deployments) time.Duration {
	if spec.ScaleDownAfter.Duration != 0 {
		return spec.ScaleDownAfter.Duration
	}
	return reviewApp.Spec.ScaleDownAfter.Duration
}

// startOnDeploy reports whether a deployment starts when the pull request is deployed
func startOnDeploy(reviewApp *reviewapps.ReviewAppConfig, spec reviewapps.Deployments) bool {
	return spec.StartOnDeploy || reviewApp.Spec.StartOnDeploy
}

// deployedAt is when the pull request was last deployed, or created when the webhook did not deploy it
func deployedAt(pr *reviewapps.PullRequest) time.Time {
	if pr.Status.DeployedAt.After(pr.CreationTimestamp.Time) {
		return pr.Status.DeployedAt.Time
	}
	return pr.CreationTimestamp.Time
}

// lastVisit is when the deployment of a Lease was last visited, or zero if it never was
func lastVisit(lease *coordinationv1.Lease) time.Time {
	if lease == nil || lease.Spec.RenewTime == nil {
		return time.Time{}
	}
	return lease.Spec.RenewTime.Time
}

// runsUntil reports whether a deployment runs at now, and until when when it scales down. It runs
// after it was visited, or deployed when it starts on deploy, until scaleDownAfter has passed
func runsUntil(lastActive time.Time, scaleDownAfter time.Duration, now time.Time) (bool, time.Time) {
	if lastActive.IsZero() {
		return false, time.Time{}
	}
	if scaleDownAfter == 0 {
		return true, time.Time{}
	}

	until := lastActive.Add(scaleDownAfter)
	return now.Before(until), until
}

// lastActive is when a deployment was last visited, or deployed when it starts on deploy
func lastActive(lease *coordinationv1.Lease, pr *reviewapps.PullRequest, startOnDeploy bool) time.Time {
	last := lastVisit(lease)
	if startOnDeploy {
		if deployed := deployedAt(pr); deployed.After(last) {
			last = deployed
		}
	}
	return last
}

// leaseDuration is the duration of the Lease of a deployment, which is unset when it never scales down
func leaseDuration(scaleDownAfter time.Duration) *int32 {
	if scaleDownAfter <= 0 {
		return nil
	}
	seconds := int32(max(scaleDownAfter.Round(time.Second)/time.Second, 1))
	return &seconds
}

// leaseHeld reports whether a Lease keeps its deployment running at now
func leaseHeld(lease *coordinationv1.Lease, now time.Time) bool {
	if lease.Spec.RenewTime == nil {
		return false
	}
	if lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	return now.Before(lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second))
}

func equalInt32Ptr(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// wakeUpPredicate passes on the changes of Leases that start their deployment, and drops the
// renewals of deployments that run, which change nothing, so that visits are not reconciled
var wakeUpPredicate = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldLease, oldOk := e.ObjectOld.(*coordinationv1.Lease)
		newLease, newOk := e.ObjectNew.(*coordinationv1.Lease)
		if !oldOk || !newOk {
			return true
		}

		now := time.Now()
		return (!leaseHeld(oldLease, now) && leaseHeld(newLease, now)) ||
			!equalInt32Ptr(oldLease.Spec.LeaseDurationSeconds, newLease.Spec.LeaseDurationSeconds)
	},
}

// migratedRenewTime carries over the last activity that earlier versions recorded on the
// PullRequest, so that upgrading neither starts nor stops review apps, nor deletes them early
func migratedRenewTime(pr *reviewapps.PullRequest, deployment string, scaleDownAfter time.Duration, now time.Time) *metav1.MicroTime {
	status := pr.Status.Deployments[deployment]
	if status == nil || status.LastActive.IsZero() {
		return nil
	}

	renewTime := status.LastActive.Time
	if !status.IsActive {
		if scaleDownAfter == 0 {
			// Any renewal would start it
			return nil
		}

		// A deploy recorded activity without starting it, so it must not hold the Lease
		renewTime = minTime(renewTime, now.Add(-scaleDownAfter))
	}

	t := metav1.NewMicroTime(renewTime)
	return &t
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// reconcileLease ensures that the Lease that records the activity of a deployment exists with
// scaleDownAfter as its duration, and returns it
func (r *PullRequestReconciler) reconcileLease(
	ctx context.Context,
	reviewApp *reviewapps.ReviewAppConfig,
	pr *reviewapps.PullRequest,
	spec reviewapps.Deployments,
	name string,
	labels map[string]string,
) (*coordinationv1.Lease, error) {
	duration := leaseDuration(scaleDownAfter(reviewApp, spec))

	var lease coordinationv1.Lease
	err := r.Get(ctx, types.NamespacedName{Namespace: pr.Namespace, Name: name}, &lease)
	if apierrors.IsNotFound(err) {
		lease = coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: pr.Namespace, Labels: labels},
			Spec: coordinationv1.LeaseSpec{
				LeaseDurationSeconds: duration,
				RenewTime:            migratedRenewTime(pr, spec.Name, scaleDownAfter(reviewApp, spec), time.Now()),
			},
		}
		if err := ctrl.SetControllerReference(pr, &lease, r.Scheme); err != nil {
			return nil, err
		}
		if err := r.Create(ctx, &lease); err != nil {
			return nil, err
		}
		return &lease, nil
	}
	if err != nil {
		return nil, err
	}

	// Different branch names can be truncated to the same name
	if !metav1.IsControlledBy(&lease, pr) {
		return nil, fmt.Errorf("lease %s %w %s", name, errNotControlled, pr.Name)
	}

	if !equalInt32Ptr(lease.Spec.LeaseDurationSeconds, duration) {
		patch := client.MergeFrom(lease.DeepCopy())
		lease.Spec.LeaseDurationSeconds = duration
		if err := r.Patch(ctx, &lease, patch); err != nil {
			return nil, err
		}
	}

	return &lease, nil
}

// apiReader reads from the API server, for the decisions that a cache that has not seen the last
// visit yet would get wrong
func (r *PullRequestReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=reviewapps.william.nu,resources=pullrequests,verbs=delete

// deleteInactive deletes a review app that has neither been deployed nor visited for deleteAfter,
// like when the webhook could not delete it as the pull request closed. It reports whether it
// deleted it, and otherwise when to look again
func (r *PullRequestReconciler) deleteInactive(
	ctx context.Context,
	reviewApp *reviewapps.ReviewAppConfig,
	pr *reviewapps.PullRequest,
	leaseNames []string,
	lastActivity time.Time,
	now time.Time,
) (bool, time.Duration, error) {
	deleteAfter := reviewApp.Spec.DeleteAfter.Duration
	if deleteAfter == 0 || !pr.DeletionTimestamp.IsZero() {
		return false, 0, nil
	}
	if expires := lastActivity.Add(deleteAfter); now.Before(expires) {
		return false, expires.Sub(now) + time.Second, nil
	}

	// Look again at the API server, which has the visits that the cache may not have seen yet
	var fresh reviewapps.PullRequest
	if err := r.apiReader().Get(ctx, client.ObjectKeyFromObject(pr), &fresh); err != nil {
		return false, 0, client.IgnoreNotFound(err)
	}
	lastActivity = deployedAt(&fresh)
	for _, name := range leaseNames {
		var lease coordinationv1.Lease
		if err := r.apiReader().Get(ctx, types.NamespacedName{Namespace: pr.Namespace, Name: name}, &lease); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, 0, err
		}
		if visit := lastVisit(&lease); visit.After(lastActivity) {
			lastActivity = visit
		}
	}
	if expires := lastActivity.Add(deleteAfter); now.Before(expires) {
		return false, expires.Sub(now) + time.Second, nil
	}

	// The preconditions keep a review app that was deployed since it was read
	err := r.Delete(ctx, &fresh, client.Preconditions{UID: &fresh.UID, ResourceVersion: &fresh.ResourceVersion})
	if apierrors.IsConflict(err) {
		return false, time.Second, nil
	}
	if err := client.IgnoreNotFound(err); err != nil {
		return false, 0, err
	}

	ctrl.LoggerFrom(ctx).Info("Deleted inactive review app",
		"branch", pr.Spec.BranchName, "lastActivity", lastActivity, "deleteAfter", deleteAfter)
	return true, 0, nil
}
