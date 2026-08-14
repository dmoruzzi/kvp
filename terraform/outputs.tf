output "dashboard_url" {
  description = "URL of the KVP dashboard."
  value       = "https://${var.grafana_cloud_region}.grafana.net/d/${grafana_dashboard.kvp.uid}"
}

output "rule_group_name" {
  description = "Name of the provisioned alert rule group."
  value       = grafana_rule_group.kvp.name
}
