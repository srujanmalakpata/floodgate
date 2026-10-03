output "alb_dns_name" {
  description = "Public DNS name of the load balancer."
  value       = aws_lb.this.dns_name
}

output "redis_primary_endpoint" {
  description = "ElastiCache primary endpoint (private)."
  value       = aws_elasticache_replication_group.this.primary_endpoint_address
}

output "log_group_name" {
  description = "CloudWatch log group with the gateway's JSON logs."
  value       = aws_cloudwatch_log_group.gateway.name
}

output "ecs_cluster_name" {
  value = aws_ecs_cluster.this.name
}

output "ecs_service_name" {
  value = aws_ecs_service.gateway.name
}

output "gateway_config" {
  description = "The rendered gateway config (contains no secrets)."
  value       = local.gateway_config
}
