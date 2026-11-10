# Notifications, as each source documents them

Each file has the shape its source's documentation gives, with the values
changed to the tests' world: the rule `Errors`, its firing for
`service=checkout` from 2026-11-02 06:02 UTC.

| File | Source | Documentation |
|---|---|---|
| `alertmanager.json` | Alertmanager 0.34 webhook, with `notification_reason` | https://github.com/prometheus/alertmanager/blob/v0.34.2/docs/configuration.md (`<webhook_config>`), reasons from https://github.com/prometheus/alertmanager/blob/v0.34.2/notify/notify.go |
| `alertmanager-0.27.json` | Alertmanager before 0.32, without it | as above |
| `grafana.json` | Grafana webhook contact point, `version: "1"` | https://grafana.com/docs/grafana/latest/alerting/configure-notifications/manage-contact-points/integrations/webhook-notifier/ |
| `amp-sns-message.json` | the SNS message's `Message`, as AWS's JSON template for Amazon Managed Service for Prometheus writes it (times as Go writes them) | https://docs.aws.amazon.com/prometheus/latest/userguide/AMP-alertmanager-receiver-JSON.html; the SNS envelope round it, signed in the test: https://docs.aws.amazon.com/sns/latest/dg/http-notification-json.html |
| `azure-fired.json`, `azure-resolved.json` | Azure Monitor common alert schema, a Prometheus rule group's alert | https://learn.microsoft.com/en-us/azure/azure-monitor/alerts/alerts-common-schema (the sample payload's essentials, the sample Prometheus alert's context) |
| `google.json`, `google-renotify.json` | Cloud Monitoring webhook, schema 1.2, a policy with a PromQL condition | https://cloud.google.com/monitoring/support/notification-options |
| `cloudwatch-alarm.json`, `cloudwatch-contributor.json` | a CloudWatch metric alarm's message to SNS, and one contributor's of an alarm on several series | https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/Notify_Users_Alarm_Changes.html, https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/alarm-actions.html |
| `eventbridge-alarm.json`, `eventbridge-contributor.json` | a CloudWatch alarm's state change, and a contributor's, as EventBridge writes them | https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/cloudwatch-and-eventbridge.html |
| `azure-log.json` | Azure Monitor common alert schema, a log search alert | https://learn.microsoft.com/en-us/azure/azure-monitor/alerts/alerts-common-schema |
| `google-threshold.json`, `google-absence.json`, `google-log.json` | Cloud Monitoring webhook, schema 1.2, policies on a threshold, an absence and a log match | https://cloud.google.com/monitoring/support/notification-options |
| `oci-grouped.json`, `oci-split.json` | an OCI Monitoring alarm's message, grouped across metric streams and split per stream | https://docs.oracle.com/en-us/iaas/Content/Monitoring/alarm-message-format.htm, https://docs.oracle.com/en-us/iaas/Content/Monitoring/alarm-message-examples.htm |
