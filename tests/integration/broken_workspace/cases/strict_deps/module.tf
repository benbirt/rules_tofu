module "mid" {
  source = "./mid"
}

module "net" {
  source = "git::https://example.com/acme/strict.git?ref=v1.0.0"
}
