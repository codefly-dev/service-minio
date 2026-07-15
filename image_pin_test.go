package main

import (
	"os"
	"strings"
	"testing"
)

func TestRuntimeImageIsImmutable(t *testing.T) {
	if image.Digest == "" || strings.Contains(image.Tag, "latest") {
		t.Fatalf("MinIO image is not pinned: %+v", image)
	}
}

func TestDeploymentImageIsImmutable(t *testing.T) {
	data, err := os.ReadFile("templates/deployment/kustomize/base/deployment.yaml.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	deployment := string(data)
	if strings.Contains(deployment, ":latest") || !strings.Contains(deployment, image.Digest) {
		t.Fatalf("MinIO deployment image is not digest-pinned: %s", deployment)
	}
}
