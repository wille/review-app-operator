[![GitHub release](https://img.shields.io/github/release/wille/review-app-operator.svg?style=flat-square)](https://github.com/wille/review-app-operator/releases/latest)

# Review App Operator

A Kubernetes operator for creating staging/preview environments for pull requests, aimed towards web applications and APIs, but can be used to deploy a staging instance of any containerized application.

It's intended to be the most simple solution to have fully functional dynamic staging review environments for pull requests and to not waste computing resources by automatically stopping environments that are not used and starting them again on demand.

Use the [Review App Action](#review-app-action) to keep deployments in sync with Pull Requests.

The Review App Operator consists of three components:

- **Manager**: The controller responsible for creating and managing the deployments and keeping them in sync. It starts a deployment when it is visited, scales it down when it hasn't been visited for `scaleDownAfter`, and deletes review apps that have neither been deployed nor visited for `deleteAfter`, like when the webhook couldn't delete them as their pull request closed. Deployments are not scaled down if `scaleDownAfter` is not set, and review apps not deleted if `deleteAfter` is not set
- **Forwarder**: The proxy responsible for routing traffic to the correct deployment and starting deployments on demand. WebSockets and streamed responses like server-sent events pass through. While a review app starts, browsers loading a page get a page that reloads until it has started, and other requests get `503 Service Unavailable` with `Retry-After`. Visits are recorded in a `Lease` named like the deployment, which a deployment holds for `scaleDownAfter` after its last visit: `kubectl get lease -l app.kubernetes.io/managed-by=review-app-controller` shows when each was last visited. The forwarder renews a Lease at most every tenth of `scaleDownAfter`, between 5 seconds and 5 minutes, so that visits barely write to the Kubernetes API
- **Deployment Webhook**: Receives pull request open, sync and close events from Github Actions with the [Review App Action](#review-app-action)

## Installation

### Prerequisites

- Access to a Kubernetes v1.30+ cluster.
- Familiarity with Helm charts.
- An Ingress controller like [ingress-nginx](https://github.com/kubernetes/ingress-nginx) with [cert-manager](https://cert-manager.io), or a [Gateway API](https://gateway-api.sigs.k8s.io) Gateway.

## Configuration `values.yaml`

See [values.yaml](/chart/values.yaml) for all available options

```yaml
webhook:
  # The webhook secret that will be used by the CI to trigger pull request events
  # The secret is created with a random value if it does not exist, unless `createSecret: false`
  secretKeyRef:
    name: webhook-secret
    key: webhook-secret
  host: review-app-webhooks.example.com
  ingress:
    annotations:
      cert-manager.io/cluster-issuer: letsencrypt # Use cert-manager to issue a certificate for the webhook Ingress
    tls: true # Enable TLS for the ingress
forwarder:
  hosts:
    - "*.review-apps.example.com"
  ingress:
    annotations:
      cert-manager.io/cluster-issuer: letsencrypt
    tls: true
```

> [!IMPORTANT]
>
> - The `forwarder.hosts` must be wildcard hostnames as only one Ingress is used to route traffic to the forwarder. You need to use this hostname suffix in `hostTemplates` in your `ReviewAppConfig`
> - The `webhook.host` is the host used for the [Review App Action](#review-app-action) webhook
> - Earlier charts set them in `webhook.ingress.host` and `forwarder.ingress.hosts`, which still work

### Gateway API

To route the hosts with HTTPRoutes of a [Gateway API](https://gateway-api.sigs.k8s.io) Gateway instead of Ingresses:

```yaml
webhook:
  host: review-app-webhooks.example.com
  ingress:
    enabled: false
  httpRoute:
    enabled: true
    parentRefs:
      - name: gateway
        namespace: gateway-system
forwarder:
  hosts:
    - "*.review-apps.example.com"
  ingress:
    enabled: false
  httpRoute:
    enabled: true
    parentRefs:
      - name: gateway
        namespace: gateway-system
```

The listeners of the Gateway terminate TLS for the hosts. The HTTPRoutes are named `<release>-webhook` and `<release>-forwarder`, like the Ingresses, to attach policies of the Gateway to.

To have the chart take over HTTPRoutes of those names that were created without Helm, label and annotate them for the release before upgrading:

```bash
$ kubectl label httproute review-app-operator-webhook review-app-operator-forwarder --namespace review-app-operator-system \
    app.kubernetes.io/managed-by=Helm
$ kubectl annotate httproute review-app-operator-webhook review-app-operator-forwarder --namespace review-app-operator-system \
    meta.helm.sh/release-name=review-app-operator meta.helm.sh/release-namespace=review-app-operator-system
```

## Install with Helm

```bash
$ helm install review-app-operator oci://ghcr.io/wille/charts/review-app-operator \
    --namespace review-app-operator-system --create-namespace \
    -f values.yaml
```

The chart installs and upgrades the `ReviewAppConfig` and `PullRequest` CRDs. Uninstalling the chart keeps them, as deleting them deletes every review app. Set `crds.install: false` to manage the CRDs yourself.

> [!IMPORTANT]
> Charts before 0.4.0 installed the CRDs without upgrading them, and told to apply them with `kubectl`. To upgrade from them, the chart takes the CRDs over once:
>
> ```bash
> $ helm upgrade review-app-operator oci://ghcr.io/wille/charts/review-app-operator \
>     --namespace review-app-operator-system -f values.yaml --take-ownership --force-conflicts
> ```
>
> `--take-ownership` lets the release own the CRDs that it didn't create, and `--force-conflicts` lets Helm 4 change the fields that `kubectl apply --server-side` set. Later upgrades need neither.

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
  connectionTimeout: 30s # Wait for review apps to start for 30 seconds before asking browsers to reload, instead of the forwarder's --connection-timeout
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

`kubectl apply` also rejects ReviewAppConfigs with:

- durations that aren't like `30m`, `2h` or `1h30m`, or are negative, in `scaleDownAfter`, `deleteAfter` and `connectionTimeout`
- deployments with the same name, or more than one without a name
- a `targetContainerPort` that isn't between 1 and 65535, or negative `replicas`

A `targetContainerName` that isn't a container of the template fails deploys, as the API server can't check it: the `Reconciled` condition of the pull requests has the reason `TargetContainerNotFound`.

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
    permissions:
      contents: read
      checks: write # To add the check with the URLs of the review app
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
      # The URLs of every deployment as ${{ fromJSON(steps.deploy.outputs.review_app_urls).test[0] }}
```

The URL of the review app is the first host template of its first deployment, which is a good link for the GitHub environment of the deploy job. The action also lists every URL of every deployment:

- in a check named `Review app <name>` that it adds to the pushed commit, which shows the URLs in its details on the pull request. When the pull request is closed, the action adds a neutral check that says that the review app was deleted
- in the summary of the job
- in the `review_app_urls` output, a JSON object of the deployment names to their URLs

The check needs the `checks: write` permission of the `github_token` input, which is the token of the workflow by default. Without it, like for pull requests from forks, the action warns that it couldn't add the check and still deploys. Set `check: false` to not add it.

### Authentication

The action authenticates deploys with the webhook secret, or, without `webhook_secret`, with the GitHub Actions ID token of the workflow.

The webhook secret is created by the Helm chart, unless `webhook.createSecret` is disabled to create it yourself:

```bash
$ kubectl get secret webhook-secret --namespace review-app-operator-system -o jsonpath='{.data.webhook-secret}' | base64 -d
```

Anyone with the webhook secret can deploy and delete every review app. An ID token only deploys and deletes the review apps of the pull requests of its own repository, of the ReviewAppConfigs that allow the repository in `repositories`, and needs no secret:

```yaml
apiVersion: reviewapps.william.nu/v1alpha1
kind: ReviewAppConfig
metadata:
  name: my-review-app
spec:
  repositories:
    - octo-org/my-app
```

```yaml
jobs:
  review-app:
    permissions:
      contents: read
      checks: write
      id-token: write # To deploy with the ID token of the workflow
    steps:
      - uses: wille/review-app-operator@master
        with:
          review_app_name: my-review-app
          review_app_namespace: staging
          webhook_url: https://review-app-webhooks.example.com
          image: user/app:${{ github.event.pull_request.head.sha }}
```

- The webhook verifies the ID tokens with the keys of GitHub Actions, which it fetches when the first deploy with an ID token arrives. Set `webhook.oidc.issuer` in the chart for a GitHub Enterprise, or empty to only accept the webhook secret.
- The tokens are requested for the audience `review-app-operator`, which the `oidc_audience` input of the action and `webhook.oidc.audience` of the chart change.
- Only `pull_request` workflows can deploy, for the branch and the pull request of their token.
- An ID token only deploys and deletes the review app of its own pull request. A pull request from a fork gets a token for the repository it is opened in, with the branch name of the fork, so it can't replace or delete the review app of another pull request whose branch has the same name. If a closed pull request left its review app behind, a new pull request of the same branch is refused until that PullRequest is deleted.
- Once every workflow deploys with ID tokens, set `webhook.acceptSecret: false` in the chart, so that the webhook secret can't deploy anymore and isn't created.

#### Pull requests from forks

A pull request from a fork can only deploy when GitHub gives its workflow an ID token or the webhook secret, which it doesn't by default. GitHub makes the write permissions of `pull_request` workflows from forks read only, and an ID token needs `id-token: write`:

- In public repositories, pull requests from forks get neither an ID token nor secrets, so they can't deploy.
- In private repositories, they get an ID token when **Send write tokens to workflows from pull requests** is enabled in the Actions settings of the repository or organization. They can then deploy and delete the review app of their own branch, with an image of the image repository of the ReviewAppConfig, and their review apps run with its env and secrets.
- **Send secrets to workflows from pull requests** gives them the webhook secret, which deploys and deletes every review app. Set `webhook.acceptSecret: false` before enabling it.
- `pull_request_target` workflows run for pull requests from forks with the permissions and secrets of the repository, so the webhook refuses their ID tokens. Don't deploy pull requests from forks in them with the webhook secret.

The ID token doesn't tell whether the branch is in a fork, so the webhook can't refuse pull requests from forks that GitHub gives an ID token.

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
NAME                     BRANCH          RECONCILED   REASON                URL                                             AGE
frontend-feature-login   feature/login   True         Reconciled            https://feature-login.review-apps.example.com   3h
frontend-new-checkout    new-checkout    False        InvalidHostTemplate                                                   2m
```

Add `-o wide` for the error message.

### Overriding environment variables

A PullRequest can override environment variables of its review app, like to point a frontend review app at the review app of a backend pull request. Overrides are set for a deployment of the ReviewAppConfig and apply to its target container, replacing the variables of the same name:

```yaml
spec:
  deployments:
    - name: test # The deployment in the ReviewAppConfig
      env:
        - name: API_URL
          value: https://api-my-branch.review-apps.example.com
```

```bash
$ kubectl patch pullrequest my-review-app-my-branch --namespace staging --type merge \
    -p '{"spec":{"deployments":[{"name":"test","env":[{"name":"API_URL","value":"https://api-my-branch.review-apps.example.com"}]}]}}'
```

- Changing the overrides restarts the review app right away, with any changes to the ReviewAppConfig it doesn't run yet. Removing an override restores the value of the ReviewAppConfig.
- Deploys keep the overrides, which are deleted with the review app when its pull request is closed.
- Overrides only take values. Values from Secrets and ConfigMaps can only be set in the ReviewAppConfig, so that whoever can edit PullRequests can't read them through a review app.
- Overriding a deployment that the ReviewAppConfig doesn't have sets the `Reconciled` condition to `False` with the reason `UnknownDeployment`, and fails the next deploy.

The PullRequest of a branch is named after the ReviewAppConfig and the branch, which is shortened and stripped of characters that names can't have. To find it by the branch name:

```bash
$ kubectl get pullrequests --namespace staging -o json | jq -r --arg branch my/branch \
    '.items[] | select(.spec.reviewAppRef == "my-review-app" and .spec.branchName == $branch) | .metadata.name'
```

Changing PullRequests needs the `patch` permission on `pullrequests`, which the chart's `pullrequest-editor-role` ClusterRole grants.

## Metrics

The controller, forwarder and webhook serve their metrics on port 8082 at `/metrics`, with those of controller-runtime. `metrics.podMonitor.enabled` creates a PodMonitor for the [Prometheus Operator](https://prometheus-operator.dev), with the `metrics.podMonitor.labels` that your Prometheus selects PodMonitors by.

| Metric | Component | Description |
| --- | --- | --- |
| `review_app_requests_total` | forwarder | Responses to requests for review apps, by status `code` |
| `review_app_start_wait_seconds` | forwarder | How long requests waited for review apps that were starting to accept connections |
| `review_app_start_timeouts_total` | forwarder | Requests that got the page that reloads, or `503 Service Unavailable`, as their review app didn't start within the connection timeout |
| `review_app_scaled_total` | controller | Deployments that the controller started (`direction="up"`) and scaled down (`direction="down"`) |
| `review_app_deleted_total` | controller | Review apps deleted after `deleteAfter` |
| `review_app_deployments` | controller | Deployments by `state`, `running` or `scaled_down`, only reported by the leading replica |
| `review_app_deploys_total` | webhook | Deploys by `result`: `succeeded`, `failed`, or `rejected` for their ID token |
| `review_app_deploy_duration_seconds` | webhook | How long successful deploys took until the review app ran the new image |

The metrics are labelled by `review_app_config` and `deployment`, the names of the ReviewAppConfig and its deployment, but never by branch. For example:

```promql
# How long visits waited for review apps to start, 90th percentile
histogram_quantile(0.9, sum by (review_app_config, le) (rate(review_app_start_wait_seconds_bucket[1d])))

# Visits that got the page that reloads, as the review app didn't start within connectionTimeout
sum by (review_app_config) (increase(review_app_start_timeouts_total[1d]))

# Running review apps
sum by (review_app_config) (review_app_deployments{state="running"})
```

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
