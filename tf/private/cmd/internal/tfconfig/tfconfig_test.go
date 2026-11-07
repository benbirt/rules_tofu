package tfconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRequiredProviders(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"versions.tf": `terraform {
  required_providers {
    aws    = { source = "HashiCorp/aws", version = ">= 5.0" }
    random = { version = "~> 3.0" }
    other  = { source = "example.com/acme/other" }
  }
}`,
		"more.tf.json": `{"terraform": [{"required_providers": [{"null": {"source": "hashicorp/null"}}]}]}`,
		"main.tf":      `resource "aws_s3_bucket" "b" {}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := RequiredProviders(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"example.com/acme/other",
		"registry.opentofu.org/hashicorp/aws",
		"registry.opentofu.org/hashicorp/null",
		"registry.opentofu.org/hashicorp/random",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestReadRequiredProviders(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"main.tf": `
terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source                = "hashicorp/aws"
      version               = ">= 6.0"
      configuration_aliases = [aws.east]
    }
    google = "~> 7.0" # legacy form: source implied
    ph = { source = "PostHog/posthog" }
  }
}

resource "null_resource" "x" {
  triggers = { a = var.undeclared }
}
`,
		"extra.tf.json": `{"terraform": {"required_providers": {"random": {"source": "hashicorp/random", "version": "3.6.0"}}}}`,
		"x_override.tf": `terraform {
  required_providers {
    random = { source = "hashicorp/random", version = "3.7.0" }
  }
}
`,
		"unlisted.tf": `terraform {
  required_providers {
    null = { source = "hashicorp/null" }
  }
}
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ReadRequiredProviders([]string{
		filepath.Join(dir, "x_override.tf"), filepath.Join(dir, "main.tf"), filepath.Join(dir, "extra.tf.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Requirement{
		"aws":    {Source: "registry.opentofu.org/hashicorp/aws", Version: ">= 6.0"},
		"google": {Source: "registry.opentofu.org/hashicorp/google", Version: "~> 7.0"},
		"ph":     {Source: "registry.opentofu.org/posthog/posthog"},
		"random": {Source: "registry.opentofu.org/hashicorp/random", Version: "3.7.0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestReadRequiredProviders_DuplicateLocalName(t *testing.T) {
	dir := t.TempDir()
	null := "terraform {\n  required_providers {\n    null = { source = \"hashicorp/null\" }\n  }\n}\n"
	for _, name := range []string{"a.tf", "b.tf"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(null), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, err := ReadRequiredProviders([]string{filepath.Join(dir, "a.tf"), filepath.Join(dir, "b.tf")})
	if err == nil || !strings.Contains(err.Error(), "already required") {
		t.Errorf("expected a duplicate error, got %v", err)
	}
}
