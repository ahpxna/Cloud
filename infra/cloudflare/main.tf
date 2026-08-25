resource "cloudflare_ruleset" "api_rate_limits" {
  zone_id     = var.zone_id
  name        = "Family Photo Cloud API rate limits"
  description = "Edge abuse controls; TUS PATCH traffic is intentionally excluded."
  kind        = "zone"
  phase       = "http_ratelimit"

  rules = [
    {
      ref         = "family_photo_cloud_login_ip"
      description = "Aggressive per-IP login throttling"
      expression  = "http.host eq \"${var.api_hostname}\" and http.request.method eq \"POST\" and http.request.uri.path eq \"/v1/auth/login\""
      action      = "block"
      ratelimit = {
        characteristics     = ["cf.colo.id", "ip.src"]
        period              = 60
        requests_per_period = 10
        mitigation_timeout  = 600
      }
    },
    {
      ref         = "family_photo_cloud_mfa_verify_ip"
      description = "Bound MFA verification attempts by IP in addition to one-time challenge attempts"
      expression  = "http.host eq \"${var.api_hostname}\" and http.request.method eq \"POST\" and http.request.uri.path eq \"/v1/auth/mfa/verify\""
      action      = "block"
      ratelimit = {
        characteristics     = ["cf.colo.id", "ip.src"]
        period              = 60
        requests_per_period = 30
        mitigation_timeout  = 300
      }
    },
    {
      ref         = "family_photo_cloud_mfa_sensitive_ip"
      description = "Bound authenticated MFA confirm/recovery/disable attempts by IP in addition to durable per-user limits"
      expression  = "http.host eq \"${var.api_hostname}\" and http.request.method eq \"POST\" and http.request.uri.path in {\"/v1/auth/mfa/confirm\" \"/v1/auth/mfa/recovery\" \"/v1/auth/mfa/disable\"}"
      action      = "block"
      ratelimit = {
        characteristics     = ["cf.colo.id", "ip.src"]
        period              = 60
        requests_per_period = 20
        mitigation_timeout  = 300
      }
    },
    {
      ref         = "family_photo_cloud_refresh_ip"
      description = "Bound refresh-token endpoint abuse"
      expression  = "http.host eq \"${var.api_hostname}\" and http.request.method eq \"POST\" and http.request.uri.path eq \"/v1/auth/refresh\""
      action      = "block"
      ratelimit = {
        characteristics     = ["cf.colo.id", "ip.src"]
        period              = 60
        requests_per_period = 60
        mitigation_timeout  = 300
      }
    },
    {
      ref         = "family_photo_cloud_upload_session_create_ip"
      description = "Moderate edge bound for upload-session creation; application also enforces durable per-user limits"
      expression  = "http.host eq \"${var.api_hostname}\" and http.request.method eq \"POST\" and http.request.uri.path eq \"/v1/upload-sessions\""
      action      = "block"
      ratelimit = {
        characteristics     = ["cf.colo.id", "ip.src"]
        period              = 60
        requests_per_period = 120
        mitigation_timeout  = 120
      }
    }
  ]
}

resource "cloudflare_ruleset" "api_method_policy" {
  zone_id     = var.zone_id
  name        = "Family Photo Cloud API method policy"
  description = "Reject methods the product protocol never uses."
  kind        = "zone"
  phase       = "http_request_firewall_custom"

  rules = [
    {
      ref         = "family_photo_cloud_tus_methods"
      description = "TUS endpoint accepts only OPTIONS, POST, HEAD, and PATCH"
      expression  = "http.host eq \"${var.api_hostname}\" and starts_with(http.request.uri.path, \"/v1/uploads\") and not http.request.method in {\"OPTIONS\" \"POST\" \"HEAD\" \"PATCH\"}"
      action      = "block"
    },
    {
      ref         = "family_photo_cloud_auth_methods"
      description = "Authentication mutation endpoints are POST-only"
      expression  = "http.host eq \"${var.api_hostname}\" and starts_with(http.request.uri.path, \"/v1/auth/\") and http.request.uri.path ne \"/v1/auth/sessions\" and not starts_with(http.request.uri.path, \"/v1/auth/sessions/\") and http.request.method ne \"POST\""
      action      = "block"
    }
  ]
}
