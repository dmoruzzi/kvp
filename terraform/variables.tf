variable "grafana_cloud_service_account_token" {
  description = "Scoped service-account token for provisioning (CI secret; separate from the runtime write-only token)."
  type        = string
  sensitive   = true
}

variable "metrics_datasource_uid" {
  description = "UID of the GrafanaCloud Metrics datasource used by dashboard panels."
  type        = string
  default     = "grafanacloud-prom"
}

variable "stack_slug" {
  description = "GrafanaCloud stack slug; the provider connects to https://<stack_slug>.grafana.net (dashboard/alerts are stack resources)."
  type        = string
}
