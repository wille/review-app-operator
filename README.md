[![GitHub release](https://img.shields.io/github/release/wille/review-app-operator.svg?style=flat-square)](https://github.com/wille/review-app-operator/releases/latest)

# Review App Operator

A Kubernetes operator for creating staging/preview environments for pull requests.

It's intended to be the most simple solution to have fully functional dynamic staging review environments for pull requests and to not waste computing resources by automatically stopping environments that are not used and starting them again on demand.

Review App Action [Review App Action](https://github.com/wille/review-app-action)

## Installation

### Prerequisites

- Access to a Kubernetes v1.11.3+ cluster.
- Familiarity with Helm charts.
- An Ingress controller like [ingress-nginx](https://github.com/kubernetes/ingress-nginx) with [cert-manager](https://cert-manager.io).

## Configuration `values.yaml`

See [values.yaml](/chart/values.yaml) for all available options

```yaml
webhook:
  secret: secret-string
  ingress:
    annotations:
      cert-manager.io/cluster-issuer: letsencrypt
    host: review-app-webhooks.example.com
forwarder:
  ingress:
    annotations:
      cert-manager.io/cluster-issuer: letsencrypt
    host: "*.review-apps.example.com"
scaleDownAfter: 1h
```

> [!IMPORTANT]
>
> - The `forwarder.ingress.host` field must be a a wildcard hostname as only one Ingress is used to route traffic to the forwarder
> - The `webhook.ingress.host` is the host used for the [Review App Action](https://github.com/wille/review-app-action) webhook

## Install with Helm

```bash
$ helm repo add review-app-operator https://wille.github.io/review-app-operator
$ helm install review-app-operator review-app-operator/review-app-operator \
    --namespace review-app-operator-system --create-namespace \
    -f values.yaml
```

## Description

The Review App Controller introduces two new Kubernetes resources: `ReviewApp` and `PullRequest`.

The `ReviewApp` is similar to a `Deployment` but with extra fields to

```yaml
apiVersion: rac.william.nu/v1alpha1
kind: ReviewApp
metadata:
  name: my-review-app
  namespace: staging
spec:
  # The domain suffix to use for the review apps
  domain: review-apps.example.com
  deployments:
    - name: test
      targetContainerName: test
      targetContainerPort: 80
      hostTemplates:
        # The host template to use for the review app
        # This will be combined with the `.spec.domain` like:
        # `test-branch-name.review-apps.example.com`
        - "test-{{branchName}}"
      template:
        spec:
          containers:
            - name: test
              image: nginx:latest
              ports:
                - containerPort: 80
```

> [!INFO]
> See the [ReviewApp sample](/config/samples/rac.william.nu_v1alpha1_reviewapp.yaml) for a more detailed example

> [!INFO]
> A Review App template may contain multiple deployments

## PullRequest

The PullRequest custom resource definition is used to create a new review app for a specific branch.

```yaml
apiVersion: rac.william.nu/v1alpha1
kind: PullRequest
metadata:
  name: my-pull-request
  namespace: staging
spec:
  # The branch name to create a review app for
  branchName: my-branch
  # The name of the review app to create
  reviewAppName: my-review-app

  imageName: <user>/<repo>:29df5a41b93906346d693c90adcd8acf266893c3@sha256:...
```

## Getting Started

## Installation

> [!TIP]
> ss

The Review App Operator consists of two main components:

- **ReviewApp**: The controller responsible for creating and managing the deployments
- **Forwarder**: The proxy responsible for starting and stopping deployments on demand
