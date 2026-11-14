terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "6.45.0"
    }
  }
}

variable "region" {
  type        = string
  description = "AWS region to deploy into."
}

variable "bucket_name" {
  type        = string
  description = "Name of the S3 bucket to manage."
}

provider "aws" {
  region = var.region

  # AWS credentials are read at plan and apply time from the usual provider
  # sources. `bazel build` runs `tofu validate` offline without contacting AWS.
}

resource "aws_s3_bucket" "example" {
  bucket = var.bucket_name
}

output "bucket_arn" {
  value = aws_s3_bucket.example.arn
}
