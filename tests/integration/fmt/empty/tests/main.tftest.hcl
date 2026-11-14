run "nested" {
  command = plan

  module {
    source = "../nested"
  }

  variables {
    name = "from the empty root"
  }
}
