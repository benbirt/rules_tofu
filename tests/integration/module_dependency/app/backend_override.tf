terraform {
  backend "local" {
    path          = "${var.state_root}/app.tfstate"
    workspace_dir = "${var.state_root}/workspaces"
  }
}
