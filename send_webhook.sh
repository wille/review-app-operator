#!/bin/bash

REVIEW_APP="reviewapp-sample"
PULL_REQUEST="pullrequest-sample"

curl localhost:8080/v1/$REVIEW_APP \
    -H "Content-Type: application/json" \
    -H "x-hub-signature-256: sha256=7b2020202022726566223a2022646576222c2020202022696d616765223a20226e67696e783a312e3139227df1ec7062157b23280cd1c1734d95bd18f0c643feb5df45427e0625b433cd8b80" \
    -X POST \
    -d @- << EOF
{
    "ref": "dev",
    "image": "nginx:1.19"
}
EOF
