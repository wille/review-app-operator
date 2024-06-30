package controller

import (

	// keda "github.com/kedacore/keda/v2/apis/keda/v1alpha1"

	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"

	williamnuv1alpha1 "github.com/wille/rac/api/v1alpha1"
	"github.com/wille/rac/internal/utils"
)

func CreatePullRequest(reviewAppRef, ref, imageTag string) error {
	ctx := context.TODO()
	log := log.Log.WithName("github")

	williamnuv1alpha1.AddToScheme(scheme.Scheme)
	c, err := client.New(config.GetConfigOrDie(), client.Options{Scheme: scheme.Scheme})

	if err != nil {
		panic(err)
	}

	reviewApp := williamnuv1alpha1.ReviewApp{}
	if err := c.Get(ctx, types.NamespacedName{Name: reviewAppRef, Namespace: "default"}, &reviewApp); err != nil {
		// Refuse to create a PullRequest if there is no valid ReviewApp in reviewAppRef
		if apierrors.IsNotFound(err) {
			log.Error(err, "unable to find review app", "name", reviewAppRef)
			return err
		}

		return err
	}

	resourceName := utils.GetResourceName(reviewAppRef, ref)

	pr := williamnuv1alpha1.PullRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resourceName,
			Namespace: "default",
		},
		Spec: williamnuv1alpha1.PullRequestSpec{
			ReviewAppRef: reviewAppRef,
		},
	}

	// if err := controllerutil.SetControllerReference(&reviewApp, &pr, scheme.Scheme); err != nil {
	// 	return err
	// }
	if err := c.Create(ctx, &pr); err != nil {
		return err
	}

	return nil
}

func Destroy(name string) {

}
