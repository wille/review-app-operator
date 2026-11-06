package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
)

// overridesAnnotation is on the Deployments that run with overrides of their PullRequest, with a
// hash of the overrides, to restart them when the overrides change
const overridesAnnotation = "reviewapps.william.nu/overrides-hash"

// errUnknownDeployment is returned when a PullRequest overrides a deployment that its
// ReviewAppConfig doesn't have, like for a misspelled deployment name
var errUnknownDeployment = errors.New("the ReviewAppConfig has no deployment")

// deploymentOverrides returns the overrides of the PullRequest for a deployment, or nil
func deploymentOverrides(pr *reviewapps.PullRequest, deploymentName string) *reviewapps.DeploymentOverrides {
	for i := range pr.Spec.Deployments {
		if pr.Spec.Deployments[i].Name == deploymentName {
			return &pr.Spec.Deployments[i]
		}
	}

	return nil
}

// overridesHash identifies the overrides that a deployment runs with. It is empty without
// overrides, like for the deployments created before there were overrides
func overridesHash(overrides *reviewapps.DeploymentOverrides) string {
	if overrides == nil || len(overrides.Env) == 0 {
		return ""
	}

	// Marshalling a list of strings can't fail
	data, _ := json.Marshal(overrides.Env)
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:8])
}

// applyOverrides sets the environment variables of the overrides in the target container,
// replacing the variables of the same name
func applyOverrides(template *corev1.PodTemplateSpec, containerName string, overrides *reviewapps.DeploymentOverrides) {
	if overrides == nil {
		return
	}

	for i := range template.Spec.Containers {
		container := &template.Spec.Containers[i]
		if container.Name != containerName {
			continue
		}

		for _, env := range overrides.Env {
			container.Env = setEnv(container.Env, corev1.EnvVar{Name: env.Name, Value: env.Value})
		}
	}
}

// setEnv sets an environment variable, replacing any of the same name
func setEnv(envs []corev1.EnvVar, env corev1.EnvVar) []corev1.EnvVar {
	found := false
	for i := range envs {
		if envs[i].Name == env.Name {
			envs[i] = env
			found = true
		}
	}

	if !found {
		envs = append(envs, env)
	}

	return envs
}

// setOverridesAnnotation records the hash of the overrides that the deployment runs with
func setOverridesAnnotation(deployment *appsv1.Deployment, hash string) {
	if hash == "" {
		delete(deployment.Annotations, overridesAnnotation)
		return
	}

	if deployment.Annotations == nil {
		deployment.Annotations = make(map[string]string)
	}
	deployment.Annotations[overridesAnnotation] = hash
}

// unknownOverrides returns an error when the PullRequest overrides a deployment that the
// ReviewAppConfig doesn't have
func unknownOverrides(reviewApp *reviewapps.ReviewAppConfig, pr *reviewapps.PullRequest) error {
	for _, overrides := range pr.Spec.Deployments {
		known := slices.ContainsFunc(reviewApp.Spec.Deployments, func(deployment reviewapps.Deployments) bool {
			return deployment.Name == overrides.Name
		})

		if !known {
			return fmt.Errorf("%w %q to override", errUnknownDeployment, overrides.Name)
		}
	}

	return nil
}
