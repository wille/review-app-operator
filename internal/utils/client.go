package utils

import (
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	williamnuv1alpha1 "github.com/wille/rac/api/v1alpha1"
)

func GetKubernetesClient() (client.Client, error) {
	williamnuv1alpha1.AddToScheme(scheme.Scheme)
	c, err := client.New(config.GetConfigOrDie(), client.Options{Scheme: scheme.Scheme})
	return c, err
}
