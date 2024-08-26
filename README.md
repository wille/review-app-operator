[![GitHub release](https://img.shields.io/github/release/wille/review-app-operator.svg?style=flat-square)](https://github.com/wille/review-app-operator/releases/latest)

# Review App Operator

A Kubernetes operator for creating staging/preview environments for pull requests, aimed towards web applications and APIs, but can be used to deploy a staging instance of any containerized application.

It's intended to be the most simple solution to have fully functional dynamic staging review environments for pull requests and to not waste computing resources by automatically stopping environments that are not used and starting them again on demand.

Use the [Review App Action](https://github.com/wille/review-app-action) to keep deployments in sync with Pull Requests.

The Review App Operator consists of four components:

- **Manager**: The controller responsible for creating and managing the deployments and keeping them in sync
- **Forwarder**: The proxy responsible for routing traffic to the correct deployment and starting deployments on demand
- **Downscaler**: Watches pull request deployments and downscales if no traffic is received after `scaleDownAfter` (default 1h)
- **Deployment Webhook**: Receives pull request open, sync and close events from Github Actions with [`wille/review-app-action`](https://github.com/wille/review-app-action)

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
      cert-manager.io/cluster-issuer: letsencrypt # Use cert-manager to issue a certificate for the webhook Ingress
    tls: true # Enable TLS for the ingress
    host: review-app-webhooks.example.com
forwarder:
  ingress:
    annotations:
      cert-manager.io/cluster-issuer: letsencrypt
    hosts:
      - "*.review-apps.example.com"
scaleDownAfter: 1h # scale down a pull request deployment if inactive for this long
```

> [!IMPORTANT]
>
> - The `forwarder.ingress.host` field must be a a wildcard hostname as only one Ingress is used to route traffic to the forwarder. You need to use this hostname suffix in `hostTemplates` in your `ReviewAppConfig`
> - The `webhook.ingress.host` is the host used for the [Review App Action](https://github.com/wille/review-app-action) webhook

## Install with Helm

```bash
$ helm repo add review-app-operator https://wille.github.io/review-app-operator
$ helm install review-app-operator review-app-operator/review-app-operator \
    --namespace review-app-operator-system --create-namespace \
    -f values.yaml
```

## Creating a Review App

The Review App Controller introduces two new Kubernetes resources: `ReviewAppConfig` and `PullRequest`.

The `ReviewAppConfig` is similar to a `Deployment` but with extra fields to select which container and port to target.

### `my-review-app.yml`

```yaml
apiVersion: reviewapps.william.nu/v1alpha1
kind: ReviewAppConfig
metadata:
  name: my-review-app
  namespace: staging
spec:
  deployments:
    - name: test
      targetContainerName: test # Update this container when a Pull request is opened or updated
      targetContainerPort: 80 # The container port to forward traffic to
      hostTemplates:
        # The host template to use for the review app
        # This will be combined with the `.spec.domain` like:
        # `test-branch-name.review-apps.example.com`
        - "test-{{.BranchName}}.review-apps.example.com"
      template:
        spec:
          containers:
            - name: test # Update this container when a Pull request is opened or updated
              image: nginx:latest
              ports:
                - containerPort: 80
            - name: redis
              image: redis:7
```

Create this ReviewAppConfig
`kubectl apply -f my-review-app.yml`

> [!NOTE]
> See the [ReviewAppConfig sample](/config/samples/reviewapps.william.nu_v1alpha1_reviewapp.yaml) for a more detailed example.
>
> Hostnames can be templated with `{{.ReviewAppConfig}}`, `{{.BranchName}}`, `{{.DeploymentName}}`, `{{.PullRequestNumber}}`

## PullRequest example

> [!IMPORTANT]
> Pull requests are created and deleted automatically by the [Review App Action](https://github.com/wille/review-app-action) that runs your Github Actions `pull_request` workflows

```yaml
apiVersion: reviewapps.william.nu/v1alpha1
kind: PullRequest
metadata:
  name: my-pull-request
  namespace: staging
spec:
  # The branch name to create a review app for
  branchName: my-branch
  # The name of the review app to create
  reviewAppName: my-review-app

  # The latest deployed image
  imageName: <user>/<repo>:29df5a41b93906346d693c90adcd8acf266893c3@sha256:...
```
