terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
  }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      "quantos.io/managed-by"  = "terraform"
      "quantos.io/environment" = var.env
      "quantos.io/repo"        = "quantos"
      # This platform is educational and paper-trading. The tag is not a
      # control, but it is the first thing a cost or security review reads.
      "quantos.io/purpose" = "research-paper-trading"
    }
  }
}
