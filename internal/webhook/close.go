package webhook

import (
	"context"
	"errors"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// errNameConflict is returned when a resource name is used by another branch, as different
// branch names can be normalized and truncated to the same resource names
var errNameConflict = errors.New("the review app name is used by another branch")

func deletePullRequestByName(client client.Client, key types.NamespacedName, branchName string) error {
	var pr reviewapps.PullRequest
	if err := client.Get(context.TODO(), key, &pr); err != nil {
		return err
	}

	if pr.Spec.BranchName != branchName {
		return errNameConflict
	}

	if err := client.Delete(context.TODO(), &pr); err != nil {
		return err
	}

	return nil
}
