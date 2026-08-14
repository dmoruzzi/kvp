terraform {
  required_version = ">= 1.5"
  required_providers {
    grafana = {
      source  = "grafana/grafana"
      version = "~> 3.0"
    }
  }
}

provider "grafana" {
  alias = "cloud"
  url   = "https://${var.stack_slug}.grafana.net"
  auth  = var.grafana_cloud_service_account_token
}
