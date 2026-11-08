package webhook

import (
	"context"
	"errors"
	"fmt"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// errNameConflict is returned when a resource name is used by another branch, as different
// branch names can be normalized and truncated to the same resource names
var errNameConflict = errors.New("the review app name is used by another branch")

// errOtherPullRequest is returned when the review app of a branch is for another pull request than
// the one a deploy is authorized for. An ID token proves the pull request of its workflow but not
// the repository of its branch, so a pull request from a fork with the branch name of another
// pull request could otherwise replace or delete the review app of that pull request
var errOtherPullRequest = errors.New("the review app of the branch is for another pull request")

// checkPullRequestNumber returns errOtherPullRequest unless the PullRequest is the review app of
// pull request number, or number is 0 for a deploy that may update the review app of any pull
// request. A PullRequest without a number, last deployed before the webhook kept the number,
// belongs to the next pull request that deploys it
func checkPullRequestNumber(pr *reviewapps.PullRequest, number int) error {
	deployed := pr.Status.PullRequestNumber
	if number == 0 || deployed == 0 || deployed == number {
		return nil
	}

	return fmt.Errorf("%w: branch %q has the review app of pull request %d, not %d. If pull request %d is closed, delete the PullRequest %s in %s",
		errOtherPullRequest, pr.Spec.BranchName, deployed, number, deployed, pr.Name, pr.Namespace)
}

// deletePullRequestByName deletes the review app of the branch, if it is the review app of pull
// request number, or of any pull request when number is 0
func deletePullRequestByName(c client.Client, key types.NamespacedName, branchName string, number int) error {
	var pr reviewapps.PullRequest
	if err := c.Get(context.TODO(), key, &pr); err != nil {
		return err
	}

	if pr.Spec.BranchName != branchName {
		return errNameConflict
	}

	if err := checkPullRequestNumber(&pr, number); err != nil {
		return err
	}

	// Only delete the PullRequest that was checked, not one that replaced it since
	return c.Delete(context.TODO(), &pr, client.Preconditions{UID: &pr.UID})
}
