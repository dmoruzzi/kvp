resource "grafana_dashboard" "kvp" {
  provider    = grafana.cloud
  folder      = grafana_folder.kvp.id
  config_json = templatefile("${path.module}/dashboards/kvp.json.tftpl", {
    datasource_uid = var.metrics_datasource_uid
  })
}

resource "grafana_folder" "kvp" {
  provider = grafana.cloud
  title    = "KVP"
}
