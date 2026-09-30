# Kubernetes workload definitions: Go control plane, event dispatcher, venue
# adapters, and the isolated research pool.
#
# TRACEABILITY
#   19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "The minimum application footprint is
#     three Go API replicas (2 vCPU / 4 GiB each), two event-dispatcher replicas
#     (2 vCPU / 4 GiB each), and at least one isolated adapter worker per enabled
#     venue (2 vCPU / 4 GiB each, concurrency capped by venue rules). Python
#     research/backtest workers use a separate quota-controlled pool and cannot
#     consume reserved API, risk, OMS, or adapter capacity."
#   19 §3: "API replicas have a minimum of three in production."
#   19 §3: "Autoscale stateless API/worker pools on sustained CPU >65% or queue
#     lag above the documented threshold for five minutes, with scale-in only
#     after ten minutes below 35% and with in-flight work drained."
#   19 §1: "Go API instances and workers run as non-root OCI containers on a
#     managed container platform."
#   06_SECURITY_AND_ACCESS_CONTROL.md §1: zero trust, mTLS, private service
#     traffic.
#   25_DEEP_ARCHITECTURAL_AUDIT_AND_FULL_SCALE_RELEASE_PROFILE.md §4: "Research and
#     backtest workers | Python | Offline experiments... | Live credentials, direct
#     authoritative writes, self-promotion to production".
#   24_ENTERPRISE_RELEASE_STANDARD.md §16: "No production credentials in research
#     or development environments."

terraform {
  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "= 2.36.0"
    }
  }
}

# --- Security posture applied to EVERY pod template --------------------------
#
# 06_SECURITY_AND_ACCESS_CONTROL.md §1: zero trust, no request trusted by
#   network position.
# 06 §4: "Every service and worker has a unique workload identity bound to its
#   deployment and environment. Use short-lived workload credentials and mutual
#   TLS for private service-to-service calls... No static shared service secrets,
#   user tokens in queues, or credentials embedded in container images. Python
#   research workers have no live environment identity."
# 19 §1: "Go API instances and workers run as non-root OCI containers."
# 25 §4: research workers must not hold live credentials.

locals {
  # Common hardened pod security context. Applied to every workload in this
  # module. Infra invariant tests assert these fields are present and that
  # privileged/allowPrivilegeEscalation are false.
  pod_security_context = {
    runAsNonRoot             = true # 19 §1: non-root OCI containers
    runAsUser                = 65532 # distroless nonroot
    runAsGroup               = 65532
    fsGroup                  = 65532
    seccompProfile = { # 06 §1 zero trust; seccomp is a required workload control
      type = "RuntimeDefault"
    }
  }

  container_security_context = {
    allowPrivilegeEscalation = false # never privileged
    privileged               = false # no privileged containers
    readOnlyRootFilesystem   = true # read-only root filesystem
    runAsNonRoot             = true
    capabilities = { # all capabilities dropped
      drop = ["ALL"]
    }
    seccompProfile = {
      type = "RuntimeDefault"
    }
  }

  # Workload-scoped environment configuration. Secrets are referenced by
  # secret-store identifiers, never embedded. 17_CONFIGURATION_AND_RISK_POLICY.md
  # §1: "Secrets are referenced by secret-store identifiers, never embedded in
  # configuration documents." 06 §6: no secrets in source, logs, images.
  common_env = [
    { name = "ENVIRONMENT", value = var.environment_name },
    { name = "CONFIG_NAMESPACE", value = var.namespace },
    { name = "OTEL_EXPORTER_OTLP_ENDPOINT", value = var.otlp_endpoint },
    { name = "OTEL_SERVICE_NAME", value = "control-plane" },
  ]
}

# --- Resource quota (quota-controlled pools) --------------------------------
# 24_ENTERPRISE_RELEASE_STANDARD.md §2 cost governance: "Non-production cannot
# consume reserved critical-path capacity."
resource "kubernetes_resource_quota" "research_pool" {
  metadata {
    name      = "research-pool-quota"
    namespace = var.namespace
    labels = {
      environment = var.environment_name
      pool        = "research"
      critical    = "false"
    }
  }

  spec {
    hard = {
      "limits.cpu"    = var.research_quota_cpu
      "limits.memory" = var.research_quota_memory
      "requests.cpu"    = var.research_quota_cpu
      "requests.memory" = var.research_quota_memory
      "pods"            = "8"
    }
  }
}

# --- Go API deployment -------------------------------------------------------
resource "kubernetes_deployment" "api" {
  metadata {
    name      = "control-plane-api"
    namespace = var.namespace
    labels = {
      app         = "control-plane-api"
      environment = var.environment_name
      language    = "go"
      authority   = "authoritative"
    }
    annotations = {
      "trade.example/artifact-digest" = var.api_artifact_digest
      "trade.example/config-fingerprint" = var.config_fingerprint
    }
  }

  spec {
    replicas = var.api_replicas

    selector {
      match_labels = {
        app         = "control-plane-api"
        environment = var.environment_name
      }
    }

    strategy {
      type = "RollingUpdate"
      rolling_update {
        # 24_ENTERPRISE_RELEASE_STANDARD.md §15: "progressive deployment".
        max_surge       = "1"
        max_unavailable = "0" # never reduce authoritative API capacity below the floor mid-roll
      }
    }

    template {
      metadata {
        labels = {
          app         = "control-plane-api"
          environment = var.environment_name
        }
      }

      spec {
        security_context = local.pod_security_context
        automount_service_account_token = true # workload identity; short-lived only

        container {
          name  = "api"
          image = var.api_image

          security_context = local.container_security_context

          ports {
            container_port = 8080
            name          = "http"
          }

          env = local.common_env

          resources {
            requests = {
              cpu    = var.api_cpu_request
              memory = var.api_memory_request
            }
            limits = {
              # 19 §3: floor is 2 vCPU / 4 GiB per API replica.
              cpu    = var.api_cpu_request
              memory = var.api_memory_request
            }
          }

          # readOnlyRootFilesystem = true requires a writable scratch volume.
          volume_mounts {
            name       = "tmp"
            mount_path = "/tmp"
          }

          readiness_probe {
            http_get {
              path = "/health/ready"
              port = 8080
            }
            initial_delay_seconds = 5
            period_seconds        = 10
          }

          liveness_probe {
            http_get {
              path = "/health/live"
              port = 8080
            }
            initial_delay_seconds = 15
            period_seconds        = 20
          }

          lifecycle {
            pre_stop {
              # 19 §3: "with in-flight work drained" before scale-in.
              exec {
                command = ["/app/control-plane", "drain", "--timeout", "25s"]
              }
            }
          }
        }

        volume {
          name = "tmp"
          empty_dir {}
        }

        # Topology spread across three zones.
        # 19 §3: "Initial production sizing starts with a three-zone primary
        # region". 19 §1: managed highly available services.
        topology_spread_constraints {
          max_skew           = 1
          topology_key       = "topology.kubernetes.io/zone"
          when_unsatisfiable = "DoNotSchedule"
          label_selector {
            match_labels = {
              app         = "control-plane-api"
              environment = var.environment_name
            }
          }
        }
      }
    }
  }
}

# --- Event dispatcher deployment ---------------------------------------------
# 19 §3: "two event-dispatcher replicas (2 vCPU / 4 GiB each)".
resource "kubernetes_deployment" "event_dispatcher" {
  metadata {
    name      = "event-dispatcher"
    namespace = var.namespace
    labels = {
      app         = "event-dispatcher"
      environment = var.environment_name
      language    = "go"
      role        = "outbox-dispatch"
    }
    annotations = {
      "trade.example/artifact-digest"   = var.dispatcher_artifact_digest
      "trade.example/config-fingerprint" = var.config_fingerprint
    }
  }

  spec {
    replicas = var.dispatcher_replicas

    selector {
      match_labels = {
        app         = "event-dispatcher"
        environment = var.environment_name
      }
    }

    strategy {
      type = "RollingUpdate"
      rolling_update {
        max_surge       = "1"
        max_unavailable = "0"
      }
    }

    template {
      metadata {
        labels = {
          app         = "event-dispatcher"
          environment = var.environment_name
        }
      }

      spec {
        security_context = local.pod_security_context

        container {
          name  = "dispatcher"
          image = var.dispatcher_image

          security_context = local.container_security_context

          env = local.common_env

          resources {
            requests = {
              cpu    = var.dispatcher_cpu_request
              memory = var.dispatcher_memory_request
            }
            limits = {
              cpu    = var.dispatcher_cpu_request
              memory = var.dispatcher_memory_request
            }
          }

          volume_mounts {
            name       = "tmp"
            mount_path = "/tmp"
          }
        }

        volume {
          name = "tmp"
          empty_dir {}
        }
      }
    }
  }
}

# --- Per-venue adapter workers ------------------------------------------------
# 19 §3: "at least one isolated adapter worker per enabled venue (2 vCPU / 4 GiB
# each, concurrency capped by venue rules)".
# 19 §3: "Adapter concurrency and request rates are governed by per-venue limits,
# not generic autoscaling."
# 16_MARKET_DATA_AND_VENUE_ADAPTERS.md §4: "Credentials are least-privilege,
# environment-scoped, and never exposed to strategy or research processes."
resource "kubernetes_deployment" "adapter" {
  for_each = var.adapter_images

  metadata {
    name      = "adapter-${each.value.venue_id}"
    namespace = var.namespace
    labels = {
      app         = "adapter"
      venue       = each.value.venue_id
      market      = each.value.market_class
      environment = var.environment_name
      language    = "go"
      authority   = "observation-only"
    }
    annotations = {
      "trade.example/venue-id"           = each.value.venue_id
      "trade.example/concurrency-cap"    = tostring(each.value.concurrency_cap)
    }
  }

  spec {
    # 19 §3: at least one isolated adapter worker per enabled venue. Not
    # autoscaled on generic CPU: per-venue limits govern concurrency.
    replicas = 1

    selector {
      match_labels = {
        app         = "adapter"
        venue       = each.value.venue_id
        environment = var.environment_name
      }
    }

    template {
      metadata {
        labels = {
          app         = "adapter"
          venue       = each.value.venue_id
          environment = var.environment_name
        }
      }

      spec {
        security_context = local.pod_security_context
        automount_service_account_token = true

        container {
          name  = "adapter"
          image = each.value.image

          security_context = local.container_security_context

          env = concat(local.common_env, [
            {
              name  = "VENUE_ID"
              value = each.value.venue_id
            },
            {
              # 19 §3: "concurrency capped by venue rules"; 16 §5: per-venue
              # rate limits, lot size, tick size are adapter capability data.
              name  = "VENUE_CONCURRENCY_CAP"
              value = tostring(each.value.concurrency_cap)
            },
          ])

          resources {
            requests = {
              cpu    = var.adapter_cpu_request
              memory = var.adapter_memory_request
            }
            limits = {
              cpu    = var.adapter_cpu_request
              memory = var.adapter_memory_request
            }
          }

          volume_mounts {
            name       = "tmp"
            mount_path = "/tmp"
          }
        }

        volume {
          name = "tmp"
          empty_dir {}
        }
      }
    }
  }
}

# --- Research pool (quota-isolated, no live identity) ------------------------
# 19 §3: "Python research/backtest workers use a separate quota-controlled pool and
# cannot consume reserved API, risk, OMS, or adapter capacity."
# 25 §4: research workers are explicitly prohibited from holding live credentials.
# 06 §4: "Python research workers have no live environment identity."
resource "kubernetes_deployment" "research" {
  metadata {
    name      = "research-workers"
    namespace = var.namespace
    labels = {
      app         = "research"
      environment = var.environment_name
      language    = "python"
      authority   = "none"
      live_identity = "prohibited"
    }
  }

  spec {
    replicas = var.research_replicas

    selector {
      match_labels = {
        app         = "research"
        environment = var.environment_name
      }
    }

    template {
      metadata {
        labels = {
          app         = "research"
          environment = var.environment_name
        }
      }

      spec {
        security_context = local.pod_security_context

        # 06_SECURITY_AND_ACCESS_CONTROL.md §4: Python research workers have no
        # live environment identity. No service account token is mounted, so the
        # research pool cannot call any cloud API, including the venue path.
        automount_service_account_token = false

        container {
          name  = "research"
          image = var.research_image

          security_context = local.container_security_context

          env = [
            {
              name  = "ENVIRONMENT"
              value = var.environment_name
            },
            {
              name  = "RESEARCH_LIVE_CREDENTIALS"
              value = "prohibited" # 25 §4, 06 §4
            },
          ]

          resources {
            requests = {
              cpu    = "2"
              memory = "4Gi"
            }
            limits = {
              cpu    = "2"
              memory = "4Gi"
            }
          }

          volume_mounts {
            name       = "tmp"
            mount_path = "/tmp"
          }
        }

        volume {
          name = "tmp"
          empty_dir {}
        }
      }
    }
  }
}

# --- PodDisruptionBudgets ----------------------------------------------------
# Financial writes remain on the authoritative primary; a voluntary disruption
# must not take the API below its minimum. 19 §3: "API replicas have a minimum of
# three in production."
resource "kubernetes_pod_disruption_budget" "api" {
  metadata {
    name      = "control-plane-api"
    namespace = var.namespace
  }

  spec {
    min_available = "2" # allows one of three API replicas to drain
    selector {
      match_labels = {
        app         = "control-plane-api"
        environment = var.environment_name
      }
    }
  }
}

resource "kubernetes_pod_disruption_budget" "dispatcher" {
  metadata {
    name      = "event-dispatcher"
    namespace = var.namespace
  }

  spec {
    min_available = "1" # 19 §3: two dispatcher replicas
    selector {
      match_labels = {
        app         = "event-dispatcher"
        environment = var.environment_name
      }
    }
  }
}

# --- Autoscaling -------------------------------------------------------------
# 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "Autoscale stateless API/worker pools
# on sustained CPU >65% or queue lag above the documented threshold for five
# minutes, with scale-in only after ten minutes below 35% and with in-flight work
# drained."
resource "kubernetes_horizontal_pod_autoscaler" "api" {
  metadata {
    name      = "control-plane-api"
    namespace = var.namespace
  }

  spec {
    scale_target_ref {
      api_version = "apps/v1"
      kind        = "Deployment"
      name        = kubernetes_deployment.api.metadata[0].name
    }

    min_replicas = var.api_replicas # never below the 19 §3 floor of three
    max_replicas = var.api_max_replicas

    metric {
      type = "Resource"
      resource {
        name = "cpu"
        target {
          type                = "Utilization"
          average_utilization = var.autoscale_scale_out_cpu_percent
        }
      }
    }

    behavior {
      scale_up {
        stabilization_window_seconds = var.autoscale_stabilization_minutes * 60
        policy {
          type           = "Percent"
          value          = 100
          period_seconds = 60
        }
      }

      scale_down {
        # 19 §3: "scale-in only after ten minutes below 35% and with in-flight
        # work drained". The ten-minute window is the safety margin against
        # flapping; the drain is the deployment preStop hook above.
        stabilization_window_seconds = 600
        policy {
          type           = "Pods"
          value          = 1
          period_seconds = 60
        }
      }
    }
  }
}

# --- Autoscaling -------------------------------------------------------------
# 19_DEPLOYMENT_TOPOLOGY_AND_SIZING.md §3: "Autoscale stateless API/worker pools
# on sustained CPU >65% or queue lag above the documented threshold for five
# minutes, with scale-in only after ten minutes below 35% and with in-flight work
# drained."
