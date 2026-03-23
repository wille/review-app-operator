#!/bin/bash

VERSION=0.0.1

set -o pipefail

TMP=${RUNNER_TEMP:-/tmp/review-app-action}
mkdir -p "$TMP" || (echo "Failed to mkdir $TMP" && exit 1)

[[ "$GITHUB_EVENT_NAME" != "pull_request" ]] && echo "Only 'pull_request' workflows are supported" && exit 1
[[ -z "$GITHUB_EVENT_PATH" ]] && echo "Missing GITHUB_EVENT_PATH" && exit 1
[[ -z "$INPUT_WEBHOOK_SECRET" ]] && echo "Missing INPUT_WEBHOOK_SECRET" && exit 1
[[ -z "$INPUT_WEBHOOK_URL" ]] && echo "Missing INPUT_WEBHOOK_URL" && exit 1
[[ -z "$INPUT_REVIEW_APP_NAME" ]] && echo "Missing INPUT_REVIEW_APP_NAME" && exit 1

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

webhook_data=$(cat <<EOF | jq
{
    "reviewAppName": "$INPUT_REVIEW_APP_NAME",
    "reviewAppNamespace": "$INPUT_REVIEW_APP_NAMESPACE",
    "repositoryUrl": "$(read_event_field .repository.html_url)",
    "branchName": "$(read_event_field .pull_request.head.ref)",
    "pullRequestUrl": "$(read_event_field .pull_request.html_url)",
    "pullRequestNumber": $(read_event_field .number),
    "image": "$INPUT_IMAGE",
    "merged": $(read_event_field .pull_request.merged),
    "sender": "$(read_event_field .sender.html_url)"
}
EOF
)

WEBHOOK_SIGNATURE_256=$(echo -n "$webhook_data" | \
    openssl dgst -sha256 -hmac "$INPUT_WEBHOOK_SECRET" -binary | \
    xxd -p | \
    tr -d '\n'
)

if [ "$INPUT_DEBUG" = true ]; then
    echo "Webhook data: $webhook_data"
    echo "Webhook signature: $WEBHOOK_SIGNATURE_256"
fi

action=$(read_event_field '.action')
case $action in
    "opened" | "reopened" | "synchronize")
        [[ -z "$INPUT_IMAGE" ]] && echo "Missing INPUT_IMAGE" && exit 1
        curl --silent --show-error --no-buffer --fail-with-body \
            -H "Content-Type: application/json" \
            -H "User-Agent: review-app-action/$VERSION" \
            -H "X-Hub-Signature-256: sha256=$WEBHOOK_SIGNATURE_256" \
            -X POST \
            --data "$webhook_data" "$INPUT_WEBHOOK_URL/v1" \
            2>&1 | tee "$TMP/output" # Stream the response to stdout and save it so we can parse it

        curl_status=$?

        echo

        if [ $curl_status -eq 0 ]; then
            # The last line of the response is the review app URL
            review_app_url=$(tail -n1 "$TMP/output" | sed 's/Review App URL: //')
            set_output "review_app_url=$review_app_url"
        fi
        ;;
    "closed")
        curl --silent --show-error --no-buffer --fail-with-body \
            -H "Content-Type: application/json" \
            -H "User-Agent: review-app-action/$VERSION" \
            -H "X-Hub-Signature-256: sha256=$WEBHOOK_SIGNATURE_256" \
            -X DELETE \
            --data "$webhook_data" "$INPUT_WEBHOOK_URL/v1"
        curl_status=$?

        ;;
    *)
        echo "Invalid action: $INPUT_ACTION"
        exit 1
        ;;
esac

exit $curl_status
