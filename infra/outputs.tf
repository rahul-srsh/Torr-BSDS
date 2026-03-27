output "vpc_id" {
  value = aws_vpc.hopvault.id
}

output "cluster_name" {
  value = aws_ecs_cluster.hopvault.name
}

output "experiment_results_bucket_name" {
  value = aws_s3_bucket.experiment_results.bucket
}