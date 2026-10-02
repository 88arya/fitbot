output "public_ip" {
  description = "Elastic IP. Point your DOMAIN's A record here."
  value       = aws_eip.app.public_ip
}

output "instance_id" {
  description = "Set as the INSTANCE_ID variable on the GitHub production environment."
  value       = aws_instance.app.id
}

output "deploy_role_arn" {
  description = "Set as the AWS_DEPLOY_ROLE_ARN variable on the GitHub production environment."
  value       = aws_iam_role.deploy.arn
}

output "env_parameter_name" {
  description = "Set as the ENV_PARAM variable on the GitHub production environment."
  value       = aws_ssm_parameter.env.name
}

output "account_id" {
  value = data.aws_caller_identity.current.account_id
}
