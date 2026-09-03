#!/bin/bash
# Provisions the AWS resources the local stack needs (§23, §25.3).
# LocalStack runs this automatically once it reports ready.
set -euo pipefail

REGION=us-east-1
awslocal() { aws --endpoint-url=http://localhost:4566 --region "$REGION" "$@"; }

echo "==> Creating SQS queues with dead-letter redrive"

# The DLQ must exist before the main queue can reference it in a redrive policy.
awslocal sqs create-queue --queue-name otp-email-delivery-dlq >/dev/null
awslocal sqs create-queue --queue-name webhook-delivery-dlq   >/dev/null

DLQ_OTP_ARN=$(awslocal sqs get-queue-attributes \
  --queue-url "http://localhost:4566/000000000000/otp-email-delivery-dlq" \
  --attribute-names QueueArn --query 'Attributes.QueueArn' --output text)

DLQ_WEBHOOK_ARN=$(awslocal sqs get-queue-attributes \
  --queue-url "http://localhost:4566/000000000000/webhook-delivery-dlq" \
  --attribute-names QueueArn --query 'Attributes.QueueArn' --output text)

# VisibilityTimeout is the redelivery window: if a worker dies while holding a
# message, SQS makes it visible again after this and another worker picks it up
# (§23.2). It must exceed the worker's realistic processing time, or a slow-but-
# alive worker gets its message handed to a second worker and the job runs twice.
awslocal sqs create-queue --queue-name otp-email-delivery --attributes "$(cat <<JSON
{
  "VisibilityTimeout": "30",
  "MessageRetentionPeriod": "345600",
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"${DLQ_OTP_ARN}\",\"maxReceiveCount\":\"5\"}"
}
JSON
)" >/dev/null

# Webhooks get a longer budget: merchant servers being briefly down is normal,
# not exceptional, so we retry far longer before giving up (§24.4).
awslocal sqs create-queue --queue-name webhook-delivery --attributes "$(cat <<JSON
{
  "VisibilityTimeout": "60",
  "MessageRetentionPeriod": "1209600",
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"${DLQ_WEBHOOK_ARN}\",\"maxReceiveCount\":\"10\"}"
}
JSON
)" >/dev/null

echo "==> Verifying sender identity in SES"
awslocal ses verify-email-identity --email-address no-reply@paylo.test

echo "==> Creating S3 bucket for dispute evidence and audit exports"
awslocal s3api create-bucket --bucket paylo-documents >/dev/null

echo "==> Seeding Secrets Manager"
awslocal secretsmanager create-secret \
  --name paylo/card-hash-salt \
  --secret-string "dev-only-insecure-salt" >/dev/null

echo "==> LocalStack ready"
awslocal sqs list-queues
