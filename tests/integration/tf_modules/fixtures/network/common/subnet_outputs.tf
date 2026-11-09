# Reached from vpc/modules/subnet as outputs.tf, a symlink the test driver
# makes: the one kind of symlink a fetched module keeps.
output "cidr" {
  value = var.cidr
}
