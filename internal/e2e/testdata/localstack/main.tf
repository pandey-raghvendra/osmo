terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  access_key = "test"
  secret_key = "test"
  region     = "us-east-1"

  # LocalStack endpoint
  endpoints {
    s3 = "http://localhost:4566"
  }

  # Skip validation checks not relevant to LocalStack.
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  s3_use_path_style           = true
}

resource "aws_s3_bucket" "drift_test" {
  bucket = "osmo-drift-test-bucket"
  tags = {
    Env = "test"
  }
}
