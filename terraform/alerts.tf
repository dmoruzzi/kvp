resource "grafana_rule_group" "kvp" {
  provider         = grafana.cloud
  name             = "kvp-alerts"
  folder_uid       = grafana_folder.kvp.uid
  interval_seconds = 60
  org_id           = 1

  rule {
    name      = "KVP high error rate"
    condition = "C"
    for       = "5m"
    labels    = { severity = "critical" }
    annotations = {
      summary     = "KVP error rate above 5% for 5m"
      description = "Error rate is above 5% for the last 5 minutes."
    }
    data {
      ref_id         = "A"
      datasource_uid = var.metrics_datasource_uid
      relative_time_range {
        from = 600
        to   = 0
      }
      model = jsonencode({
        expr         = "sum(rate(kvp_http_errors_total[5m]))"
        instant      = true
        range        = false
        legendFormat = "__auto"
        refId        = "A"
      })
    }
    data {
      ref_id         = "B"
      datasource_uid = var.metrics_datasource_uid
      relative_time_range {
        from = 600
        to   = 0
      }
      model = jsonencode({
        expr         = "sum(rate(kvp_http_requests_total[5m]))"
        instant      = true
        range        = false
        legendFormat = "__auto"
        refId        = "B"
      })
    }
    data {
      ref_id         = "C"
      datasource_uid = "__expr__"
      model = jsonencode({
        type       = "math"
        expression = "($${A} / $${B}) > 0.05"
        hide       = false
        refId      = "C"
      })
    }
  }

  rule {
    name      = "KVP DB size near limit"
    condition = "C"
    for       = "10m"
    labels    = { severity = "warning" }
    annotations = {
      summary     = "KVP database above 90% of the configured size limit"
      description = "The database has been above 90% of its size limit for 10 minutes."
    }
    data {
      ref_id         = "A"
      datasource_uid = var.metrics_datasource_uid
      relative_time_range {
        from = 600
        to   = 0
      }
      model = jsonencode({
        expr         = "kvp_db_size_bytes"
        instant      = true
        range        = false
        legendFormat = "__auto"
        refId        = "A"
      })
    }
    data {
      ref_id         = "B"
      datasource_uid = var.metrics_datasource_uid
      relative_time_range {
        from = 600
        to   = 0
      }
      model = jsonencode({
        expr         = "kvp_db_size_limit_bytes"
        instant      = true
        range        = false
        legendFormat = "__auto"
        refId        = "B"
      })
    }
    data {
      ref_id         = "C"
      datasource_uid = "__expr__"
      model = jsonencode({
        type       = "math"
        expression = "($${A} / $${B}) > 0.9"
        hide       = false
        refId      = "C"
      })
    }
  }
}
