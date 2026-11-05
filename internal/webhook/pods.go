package webhook

import (
	"context"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// crashLoopRestarts is how many times a container must have restarted in a crash loop to
	// fail the rollout, so that a container that crashes while its dependencies start gets a chance
	crashLoopRestarts = 3

	// podLogLines and podLogBytes limit the logs of a crashing container that are reported
	podLogLines = 20
	podLogBytes = 8 << 10
)

// stuckReasons are reasons a container waits to start that it won't recover from by itself
var stuckReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
}

// podInspector finds why the new pods of a rollout fail to start
type podInspector struct {
	// reader lists the pods directly from the API server, so that the webhook doesn't cache
	// every pod in the namespaces
	reader client.Reader

	// logs reads the logs of crashing containers
	logs corev1client.PodsGetter
}

// podFailure is why a pod of a rollout fails to start
type podFailure struct {
	pod       string
	container string
	reason    string

	// logs are the last logs of a crashing container
	logs string
}

// failure returns why a pod with the new image of the deployment fails to start, or nil while
// its pods are starting
func (p podInspector) failure(ctx context.Context, deployment *appsv1.Deployment, containerName, image string) (*podFailure, error) {
	if p.reader == nil || deployment.Spec.Selector == nil {
		return nil, nil
	}

	var pods corev1.PodList
	if err := p.reader.List(ctx, &pods,
		client.InNamespace(deployment.Namespace),
		client.MatchingLabels(deployment.Spec.Selector.MatchLabels),
	); err != nil {
		return nil, err
	}

	for _, pod := range pods.Items {
		// Pods of the previous rollout may still be crashing
		if pod.DeletionTimestamp != nil || !runsImage(pod.Spec.Containers, containerName, image) {
			continue
		}

		for _, status := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
			waiting := status.State.Waiting
			if waiting == nil {
				continue
			}

			failure := &podFailure{pod: pod.Name, container: status.Name}

			switch {
			case waiting.Reason == "CrashLoopBackOff" && status.RestartCount >= crashLoopRestarts:
				failure.reason = fmt.Sprintf("container %q in pod %s is crashing, it restarted %d times", status.Name, pod.Name, status.RestartCount)
				if terminated := status.LastTerminationState.Terminated; terminated != nil {
					failure.reason += fmt.Sprintf(" and last exited with code %d (%s)", terminated.ExitCode, terminated.Reason)
				}
				failure.logs = p.lastLogs(ctx, &pod, status.Name)

				return failure, nil
			case stuckReasons[waiting.Reason]:
				failure.reason = fmt.Sprintf("container %q in pod %s can't start: %s: %s", status.Name, pod.Name, waiting.Reason, waiting.Message)

				return failure, nil
			}
		}
	}

	return nil, nil
}

// lastLogs returns the last logs of the previous run of a crashed container
func (p podInspector) lastLogs(ctx context.Context, pod *corev1.Pod, container string) string {
	if p.logs == nil {
		return ""
	}

	lines, limitBytes := int64(podLogLines), int64(podLogBytes)
	logs, err := p.logs.Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container:  container,
		Previous:   true,
		TailLines:  &lines,
		LimitBytes: &limitBytes,
	}).DoRaw(ctx)
	if err != nil {
		log.Error(err, "Failed to read the logs", "pod", pod.Name, "container", container)
		return ""
	}

	return strings.TrimRight(string(logs), "\n")
}
