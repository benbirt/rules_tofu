# Deliberately references an undeclared variable so tofu validate fails; do not
# declare it (tf_modules_test.sh, step 10).
output "echo" {
  value = var.does_not_exist
}
