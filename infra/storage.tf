resource "aws_s3_bucket" "experiment_results" {
  bucket        = local.experiment_results_bucket_name
  force_destroy = true
}

resource "aws_s3_bucket_versioning" "experiment_results" {
  bucket = aws_s3_bucket.experiment_results.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "experiment_results" {
  bucket = aws_s3_bucket.experiment_results.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "experiment_results" {
  bucket = aws_s3_bucket.experiment_results.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}
