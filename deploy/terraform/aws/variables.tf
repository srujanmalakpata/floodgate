variable "name" {
  description = "Name prefix for every resource."
  type        = string
  default     = "rlgw"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,20}$", var.name))
    error_message = "name must be 2-21 lowercase letters, digits or hyphens."
  }
}

variable "aws_region" {
  description = "AWS region, e.g. ca-central-1."
  type        = string
  default     = "ca-central-1"
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC (split into 2 public and 2 private subnets)."
  type        = string
  default     = "10.40.0.0/16"
}

variable "image" {
  description = "Gateway container image (e.g. ghcr.io/srujanmalakpata/floodgate:0.1.0)."
  type        = string
}

variable "upstream_url" {
  description = "URL of the service the gateway protects, reachable from the private subnets."
  type        = string
}

variable "upstream_port" {
  description = "TCP port of the upstream, used to scope the tasks' egress rule."
  type        = number
  default     = 8080
}

variable "desired_count" {
  description = "Initial number of gateway tasks."
  type        = number
  default     = 2
}

variable "max_count" {
  description = "Upper bound for CPU-based autoscaling."
  type        = number
  default     = 6
}

variable "task_cpu" {
  description = "Fargate task CPU units (256 = 0.25 vCPU)."
  type        = number
  default     = 256
}

variable "task_memory" {
  description = "Fargate task memory in MiB."
  type        = number
  default     = 512
}

variable "redis_node_type" {
  description = "ElastiCache node type."
  type        = string
  default     = "cache.t4g.micro"
}

variable "failure_policy" {
  description = "What the gateway does when Redis is unreachable: open, closed or local."
  type        = string
  default     = "local"

  validation {
    condition     = contains(["open", "closed", "local"], var.failure_policy)
    error_message = "failure_policy must be open, closed or local."
  }
}

variable "routes" {
  description = "Rate-limited routes, rendered into the gateway config."
  type = list(object({
    name        = string
    path_prefix = string
    algorithm   = optional(string, "token_bucket")
    limit       = number
    window      = string
    key_by      = optional(string, "ip")
    ip_limit    = optional(number, 0) # key_by api_key only: per-IP cap across all keys (0 = off)
    methods     = optional(list(string), [])
  }))
  default = [
    { name = "api", path_prefix = "/api/", limit = 100, window = "1m", key_by = "api_key", ip_limit = 300 },
  ]
}

variable "allowed_ingress_cidrs" {
  description = "CIDRs allowed to reach the public load balancer."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "certificate_arn" {
  description = "ACM certificate for an HTTPS listener. Empty = HTTP only (demo)."
  type        = string
  default     = ""
}

variable "log_retention_days" {
  description = "CloudWatch log retention."
  type        = number
  default     = 14
}

variable "tags" {
  description = "Extra tags for every resource."
  type        = map(string)
  default     = {}
}
