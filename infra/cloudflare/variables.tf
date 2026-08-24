variable "zone_id" {
  description = "Cloudflare zone ID containing the Family Photo Cloud hostname."
  type        = string
}

variable "api_hostname" {
  description = "Exact public hostname routed to cloudflared, for example photos.example.com."
  type        = string
}
