[![GitHub release](https://img.shields.io/github/release/wille/review-app-operator.svg?style=flat-square)](https://github.com/wille/review-app-operator/releases/latest)

# Review App Operator

A Kubernetes operator for creating staging/preview environments for pull requests, aimed towards web applications and APIs, but can be used to deploy a staging instance of any containerized application.

It's intended to be the most simple solution to have fully functional dynamic staging review environments for pull requests and to not waste computing resources by automatically stopping environments that are not used and starting them again on demand.

Use the [Review App Action](#review-app-action) to keep deployments in sync with Pull Requests.

The Review App Operator consists of four components:

- **Manager**: The controller responsible for creating and managing the deployments and keeping them in sync
- **Forwarder**: The proxy responsible for routing traffic to the correct deployment and starting deployments on demand. WebSockets and streamed responses like server-sent events pass through. While a review app starts, browsers loading a page get a page that reloads until it has started, and other requests get `503 Service Unavailable` with `Retry-After`
- **Downscaler**: Watches pull request deployments and downscales if no traffic is received for `scaleDownAfter`. Deployments are not downscaled if `scaleDownAfter` is not set. Review apps that have neither been deployed nor received traffic for `deleteAfter` are deleted, like when the webhook couldn't delete them as their pull request closed. The next deploy creates them again. Review apps are not deleted if `deleteAfter` is not set
- **Deployment Webhook**: Receives pull request open, sync and close events from Github Actions with the [Review App Action](#review-app-action)

## Installation

### Prerequisites

- Access to a Kubernetes v1.11.3+ cluster.
- Familiarity with Helm charts.
- An Ingress controller like [ingress-nginx](https://github.com/kubernetes/ingress-nginx) with [cert-manager](https://cert-manager.io).

## Configuration `values.yaml`

See [values.yaml](/chart/values.yaml) for all available options

```yaml
webhook:
  # The webhook secret that will be used by the CI to trigger pull request events
  # The secret is created with a random value if it does not exist, unless `createSecret: false`
  secretKeyRef:
    name: webhook-secret
    key: webhook-secret
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
```

> [!IMPORTANT]
>
> - The `forwarder.ingress.hosts` must be wildcard hostnames as only one Ingress is used to route traffic to the forwarder. You need to use this hostname suffix in `hostTemplates` in your `ReviewAppConfig`
> - The `webhook.ingress.host` is the host used for the [Review App Action](#review-app-action) webhook

## Install with Helm

```bash
$ helm install review-app-operator oci://ghcr.io/wille/charts/review-app-operator \
    --namespace review-app-operator-system --create-namespace \
    -f values.yaml
```

> [!IMPORTANT]
> Helm does not upgrade CRDs. Apply the CRDs of the new chart version before upgrading:
>
> ```bash
> $ helm show crds oci://ghcr.io/wille/charts/review-app-operator --version <version> | kubectl apply --server-side -f -
> ```

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
  scaleDownAfter: 1h # Scale down review apps that receive no traffic for an hour
  deleteAfter: 168h # Delete review apps that are neither deployed nor visited for a week
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
> See the [ReviewAppConfig sample](/config/samples/rac.william.nu_v1alpha1_reviewappconfig.yaml) for a more detailed example.
>
> Hostnames can be templated with `{{.ReviewAppConfig}}`, `{{.BranchName}}`, `{{.DeploymentName}}`, `{{.PullRequestNumber}}` and `{{.RandomID}}`, and must contain `{{.BranchName}}` or `{{.RandomID}}`. Other than the variables, host templates may only contain lowercase letters, digits, `-` and `.`, and `kubectl apply` rejects templates that don't

### Random hostnames

`{{.RandomID}}` makes review app hostnames hard to guess, like `frontend-{{.RandomID}}.review-apps.example.com` for `frontend-3f9c1a7b2d4e.review-apps.example.com`. The ID is 12 hex digits from the PullRequest's random UID:

- It is the same for every push to a pull request, and for all hostnames of the pull request.
- A pull request that is closed and reopened gets a new ID.
- Adding it to the `hostTemplates` of a `ReviewAppConfig` changes the hostnames of the existing review apps.

A long branch name is shortened to keep the random ID in the hostname. Random hostnames are hard to guess, but not secret: they appear in the pull request's deployment links and CI logs.

## Review App Action

The Review App Action deploys the pull request image when a pull request is opened or updated, and deletes the review app when it is closed. Build and push the image before the action runs.

The action runs directly on the runner and needs `bash`, `curl`, `jq` and `openssl`, which GitHub-hosted runners have.

The action waits until the review app runs the new image, and fails with the reason in the job log as soon as:

- the controller can't create or update the review app, like for an invalid host template
- a new pod keeps crashing, after 3 restarts. The last 20 lines of the container's logs are included
- a new pod can't start, like when its image can't be pulled or a secret it uses is missing

```yaml
name: Review App

on:
  pull_request:
    types: [opened, synchronize, reopened, closed]

concurrency:
  # Stop waiting for a deployment that was replaced by a newer push
  group: review-app-${{ github.head_ref }}
  cancel-in-progress: true

jobs:
  review-app:
    runs-on: ubuntu-latest
    steps:
      # Build and push user/app:${{ github.event.pull_request.head.sha }} unless the pull request is closed

      - name: Deploy
        id: deploy
        uses: wille/review-app-operator@master
        with:
          review_app_name: my-review-app
          review_app_namespace: staging
          webhook_url: https://review-app-webhooks.example.com
          webhook_secret: ${{ secrets.REVIEW_APP_WEBHOOK_SECRET }}
          image: user/app:${{ github.event.pull_request.head.sha }}

      # The URL of the review app is available as ${{ steps.deploy.outputs.review_app_url }}
```

The webhook secret is created by the Helm chart, unless `webhook.createSecret` is disabled to create it yourself:

```bash
$ kubectl get secret webhook-secret --namespace review-app-operator-system -o jsonpath='{.data.webhook-secret}' | base64 -d
```

> [!NOTE]
> The action and the operator should be updated together, older actions are rejected by newer operators

## PullRequest resource

> [!IMPORTANT]
> Pull requests are created and deleted automatically by the [Review App Action](#review-app-action) that runs your Github Actions `pull_request` workflows
>
> You can manually create PullRequest resources to test the operator

```yaml
apiVersion: reviewapps.william.nu/v1alpha1
kind: PullRequest
metadata:
  name: my-pull-request
  namespace: staging
spec:
  # The branch name to create a review app for
  branchName: my-branch
  # The ReviewAppConfig to create a review app with
  reviewAppRef: my-review-app

  # The latest deployed image
  imageName: <user>/<repo>:29df5a41b93906346d693c90adcd8acf266893c3@sha256:...
```

The `Reconciled` condition of a PullRequest tells whether the controller could create and update its review app, and why not:

```bash
$ kubectl get pullrequests --namespace staging
NAME                     BRANCH          RECONCILED   REASON                AGE
frontend-feature-login   feature/login   True         Reconciled            3h
frontend-new-checkout    new-checkout    False        InvalidHostTemplate   2m
```

Add `-o wide` for the error message.

## Releasing

The controller and the Helm chart are released separately.

### Controller

Publish a GitHub release tagged `vX.Y.Z` to build and push the controller image `ghcr.io/wille/review-app-operator:vX.Y.Z`. The tag is also the release of the [Review App Action](#review-app-action).

```bash
$ gh release create vX.Y.Z --target master --generate-notes
```

Every push to `master` also publishes the image `ghcr.io/wille/review-app-operator:master`.

### Helm chart

Bump `version` in [`chart/Chart.yaml`](/chart/Chart.yaml) and push it to `master`. The chart is published to `oci://ghcr.io/wille/charts/review-app-operator` and the commit is tagged `chart-X.Y.Z`. Published versions are never overwritten.

To make the chart deploy a newer controller by default, set `appVersion` to its release tag. The chart is only published if that controller image exists.
