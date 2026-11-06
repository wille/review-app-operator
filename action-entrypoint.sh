#!/bin/bash

VERSION=0.0.1

set -o pipefail

# The runner's temp directory is shared with the other steps of the job
response=$(mktemp "${RUNNER_TEMP:-/tmp}/review-app-action.XXXXXX") || { echo "Failed to create a temp file"; exit 1; }
check_response=$(mktemp "${RUNNER_TEMP:-/tmp}/review-app-action.XXXXXX") || { echo "Failed to create a temp file"; exit 1; }
trap 'rm -f "$response" "$check_response"' EXIT

[[ "$GITHUB_EVENT_NAME" != "pull_request" ]] && echo "Only 'pull_request' workflows are supported" && exit 1
[[ -z "$GITHUB_EVENT_PATH" ]] && echo "Missing GITHUB_EVENT_PATH" && exit 1
[[ -z "$INPUT_WEBHOOK_URL" ]] && echo "Missing INPUT_WEBHOOK_URL" && exit 1
[[ -z "$INPUT_REVIEW_APP_NAME" ]] && echo "Missing INPUT_REVIEW_APP_NAME" && exit 1

# The action runs directly on the runner
for tool in curl jq openssl; do
    command -v "$tool" > /dev/null || { echo "Missing $tool on the runner"; exit 1; }
done

github_event_data=$(cat "$GITHUB_EVENT_PATH")

read_event_field() {
    echo "$github_event_data" | jq -r "$1"
}

set_output() {
    if [ -z "$GITHUB_OUTPUT" ]; then
        echo "Action output: $1"
    else
        echo "$1" >> "$GITHUB_OUTPUT"
    fi
}

# review_app_urls prints the URLs of every deployment as a JSON object of the deployment names to
# their URLs, from the "URL of <deployment>: <url>" lines of the response. Older operators only
# report the URL of the review app, which is passed as $1
review_app_urls() {
    grep -E '^URL of .*: https?://' "$response" | jq -R -s -c --arg url "$1" '
        split("\n")
        | map(select(. != "") | capture("^URL of (?<deployment>.*): (?<url>https?://.*)$"))
        | if length == 0 then {"": [$url]} else reduce .[] as $u ({}; .[$u.deployment] += [$u.url]) end
    '
}

# urls_markdown lists the URLs of $1 by deployment in Markdown
urls_markdown() {
    echo "$1" | jq -r '
        to_entries
        | map((if .key == "" then "" else "**\(.key)**\n\n" end) + (.value | map("- \(.)") | join("\n")))
        | join("\n\n")
    '
}

# write_summary adds the Markdown $1 to the summary of the job
write_summary() {
    [ -z "$GITHUB_STEP_SUMMARY" ] && return 0
    printf '### Review app `%s`\n\n%s\n' "$INPUT_REVIEW_APP_NAME" "$1" >> "$GITHUB_STEP_SUMMARY"
}

# add_check adds a completed check named after the review app to the head commit of the pull
# request, with the conclusion $1, the title $2 and the Markdown summary $3. A check that can't
# be added, like without the checks: write permission, doesn't fail the action
add_check() {
    [[ "$INPUT_CHECK" == "false" ]] && return 0

    if [ -z "$INPUT_GITHUB_TOKEN" ]; then
        echo "::warning::No github_token to add the review app check with"
        return 0
    fi

    local check repository
    check=$(jq -n \
        --arg name "Review app $INPUT_REVIEW_APP_NAME" \
        --arg sha "$(read_event_field '.pull_request.head.sha')" \
        --arg conclusion "$1" \
        --arg title "$2" \
        --arg summary "$3" \
        '{name: $name, head_sha: $sha, status: "completed", conclusion: $conclusion, output: {title: $title, summary: $summary}}'
    ) || { echo "::warning::Failed to create the review app check"; return 0; }
    repository=$(read_event_field '.repository.full_name')

    if ! curl --silent --show-error --fail-with-body --output "$check_response" \
        -H "Accept: application/vnd.github+json" \
        -H "Authorization: Bearer $INPUT_GITHUB_TOKEN" \
        -H "X-GitHub-Api-Version: 2022-11-28" \
        -H "User-Agent: review-app-action/$VERSION" \
        -X POST \
        --data "$check" "${GITHUB_API_URL:-https://api.github.com}/repos/$repository/check-runs"; then
        echo "::warning::Failed to add the review app check, which needs the checks: write permission: $(jq -r '.message // empty' "$check_response" 2>/dev/null)"
    fi
}

action=$(read_event_field '.action')
case $action in
    "opened" | "reopened" | "synchronize")
        method=POST
        [[ -z "$INPUT_IMAGE" ]] && echo "Missing INPUT_IMAGE" && exit 1
        ;;
    "closed")
        method=DELETE
        ;;
    *)
        echo "Invalid action: $action"
        exit 1
        ;;
esac

# The method and timestamp are signed with the payload so that it can't be
# replayed with another method or later on.
# Values are passed as jq arguments, as branch names may contain quotes
webhook_data=$(jq \
    --arg method "$method" \
    --argjson timestamp "$(date +%s)" \
    --arg reviewAppName "$INPUT_REVIEW_APP_NAME" \
    --arg reviewAppNamespace "$INPUT_REVIEW_APP_NAMESPACE" \
    --arg image "$INPUT_IMAGE" \
    '{
        method: $method,
        timestamp: $timestamp,
        reviewAppName: $reviewAppName,
        reviewAppNamespace: $reviewAppNamespace,
        repositoryUrl: .repository.html_url,
        branchName: .pull_request.head.ref,
        pullRequestUrl: .pull_request.html_url,
        pullRequestNumber: .number,
        image: $image,
        merged: .pull_request.merged,
        sender: .sender.html_url
    }' "$GITHUB_EVENT_PATH"
) || { echo "Failed to create the webhook payload"; exit 1; }

# Deploys authenticate with the webhook secret, or with the GitHub Actions ID token of the workflow
if [ -n "$INPUT_WEBHOOK_SECRET" ]; then
    WEBHOOK_SIGNATURE_256=$(echo -n "$webhook_data" | \
        openssl dgst -sha256 -hmac "$INPUT_WEBHOOK_SECRET" -r | \
        cut -d ' ' -f 1
    ) || { echo "Failed to sign the webhook payload"; exit 1; }
    auth_header="X-Hub-Signature-256: sha256=$WEBHOOK_SIGNATURE_256"
else
    if [ -z "$ACTIONS_ID_TOKEN_REQUEST_URL" ] || [ -z "$ACTIONS_ID_TOKEN_REQUEST_TOKEN" ]; then
        echo "Set webhook_secret, or give the job the id-token: write permission to deploy with a GitHub Actions ID token"
        exit 1
    fi

    audience=$(jq -rn --arg audience "${INPUT_OIDC_AUDIENCE:-review-app-operator}" '$audience | @uri')
    id_token=$(curl --silent --show-error --fail \
        -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
        "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=$audience" | jq -r '.value // empty')
    [ -z "$id_token" ] && { echo "Failed to get a GitHub Actions ID token"; exit 1; }
    echo "::add-mask::$id_token"
    auth_header="Authorization: Bearer $id_token"
fi

# Streams the response to stdout and saves it so we can parse it
curl --silent --show-error --no-buffer --fail-with-body \
    -H "Content-Type: application/json" \
    -H "User-Agent: review-app-action/$VERSION" \
    -H "$auth_header" \
    -X "$method" \
    --data "$webhook_data" "$INPUT_WEBHOOK_URL/v1" \
    2>&1 | tee "$response"

curl_status=$?

echo

[ $curl_status -ne 0 ] && exit $curl_status

case $method in
    POST)
        # The last line of a successful response is the review app URL.
        # Errors after the response has started streaming can't change the
        # status code, so anything else means the deployment failed
        last_line=$(tail -n1 "$response")
        if [[ "$last_line" != "Review App URL: "* ]]; then
            echo "Deployment failed"
            exit 1
        fi

        review_app_url=${last_line#Review App URL: }
        urls=$(review_app_urls "$review_app_url")
        set_output "review_app_url=$review_app_url"
        set_output "review_app_urls=$urls"

        markdown=$(urls_markdown "$urls")
        write_summary "$markdown"

        title=$review_app_url
        count=$(echo "$urls" | jq '[.[] | length] | add')
        [ "$count" -gt 1 ] && title="$review_app_url and $((count - 1)) more"
        add_check success "$title" "$markdown"
        ;;
    DELETE)
        # Closing a pull request that has no review app leaves no check to replace
        if grep -q '^Pull request deleted' "$response"; then
            add_check neutral "Deleted" "The review app was deleted when the pull request was closed."
        fi
        ;;
esac

exit 0
