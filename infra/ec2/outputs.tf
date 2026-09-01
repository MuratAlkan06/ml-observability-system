# Outputs are limited to the two stable identifiers (docs/PLAN.md D10).
#
# Public IP and public DNS are deliberately absent. The host is started and
# stopped between demos and takes a new public address each time, so an output
# carrying one would be stale the moment it was written down — and quoting it
# in a README would be a claim that does not survive the next stop/start.

output "instance_id" {
  description = "Id of the shadow-test EC2 host."
  value       = aws_instance.app.id
}

output "sg_id" {
  description = "Id of the security group attached to the shadow-test host."
  value       = aws_security_group.app.id
}
