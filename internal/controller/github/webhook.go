package github

import (

	// keda "github.com/kedacore/keda/v2/apis/keda/v1alpha1"

	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"

	williamnuv1alpha1 "github.com/wille/rac/api/v1alpha1"
)

func Create(name string, image string) error {
	ctx := context.TODO()
	log := log.Log.WithName("github")

	williamnuv1alpha1.AddToScheme(scheme.Scheme)

	c, err := client.New(config.GetConfigOrDie(), client.Options{Scheme: scheme.Scheme})

	if err != nil {
		panic(err)
	}

	reviewApp := williamnuv1alpha1.ReviewApp{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &reviewApp); err != nil {
		if apierrors.IsNotFound(err) {
			log.Error(err, "unable to find review app", "name", name)
			return err
		}

		return err
	}

	reviewApp.Status.ActiveApps = append(reviewApp.Status.ActiveApps, name)
	for _, app := range reviewApp.Spec.Deployments {
		app.Spec.Containers[0].Image = image
	}
	c.Update(ctx, &reviewApp)

	if err := c.Status().Update(ctx, &reviewApp); err != nil {
		log.Error(err, "unable to update review app status", "reviewApp", reviewApp)
	}

	return nil
}

func Destroy(name string) {

}
