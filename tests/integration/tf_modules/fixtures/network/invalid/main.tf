# A module that does not validate as published: it references a variable it
# never declares. Do not declare it (tf_modules_test.sh, step 10).
output "echo" {
  value = var.does_not_exist
}
