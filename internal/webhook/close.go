package webhook

import (
	"context"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func deletePullRequestByName(client client.Client, key types.NamespacedName) error {
	var pr reviewapps.PullRequest
	if err := client.Get(context.TODO(), key, &pr); err != nil {
		return err
	}

	if err := client.Delete(context.TODO(), &pr); err != nil {
		return err
	}

	return nil
}
