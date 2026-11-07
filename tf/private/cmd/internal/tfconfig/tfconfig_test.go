package tfconfig

import (
	"os"
	"path/filepath"
	"reflect"
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
