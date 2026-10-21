variable "region" {
  description = "AWS region."
  type        = string
  default     = "ca-central-1"
}

variable "name" {
  description = "Name prefix for all resources."
  type        = string
  default     = "fitbot"
}

variable "instance_type" {
  description = "EC2 instance type. Must be Graviton (arm64) to match the AMI."
  type        = string
  default     = "t4g.micro"
}

variable "root_volume_gb" {
  description = "Root volume size; holds Docker images and the Postgres data volume."
  type        = number
  default     = 20
}

variable "github_repo" {
  description = "GitHub repo (owner/name) allowed to deploy through OIDC."
  type        = string
  default     = "88arya/fitbot"
}

variable "create_github_oidc_provider" {
  description = "Create the GitHub Actions OIDC provider. Set false if the AWS account already has one."
  type        = bool
  default     = true
}

variable "env_parameter_name" {
  description = "SSM Parameter Store name holding the production .env file."
  type        = string
  default     = "/fitbot/prod/env"
}
