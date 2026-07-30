package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services"
	agenttesting "github.com/codefly-dev/core/agents/testing"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
)

func TestDeploymentTemplates(t *testing.T) {
	destination := agenttesting.AssertKustomizeTemplates(t, deploymentFS, &deploymentTemplateParameters{})

	secret := readDeploymentFile(t, destination, "overlays", "test", "secret.yaml")
	if !strings.Contains(secret, "kind: Secret") || !strings.Contains(secret, "c2VjcmV0") {
		t.Fatalf("ephemeral profile did not render a populated Secret:\n%s", secret)
	}
}

func TestRestrictedPortableDeploymentReferencesExternalSecretsAndReturnsValueFreeConnection(t *testing.T) {
	builder, networkMappings := newDeploymentTestBuilder(t)
	accessKeyEnv := resources.ServiceSecretConfigurationKeyFromUnique(builder.Unique(), "minio", "MINIO_ACCESS_KEY")
	secretKeyEnv := resources.ServiceSecretConfigurationKeyFromUnique(builder.Unique(), "minio", "MINIO_SECRET_KEY")
	destination := t.TempDir()

	response, err := builder.Deploy(context.Background(), restrictedDeploymentRequest(
		destination,
		networkMappings,
		map[string]*builderv0.KubernetesSecretKeyReference{
			accessKeyEnv: {Name: "minio-credentials", Key: "access-key"},
			secretKeyEnv: {Name: "minio-credentials", Key: "secret-key"},
			"UNRELATED_SECRET": {Name: "unrelated-credentials", Key: "token"},
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetState().GetState() != builderv0.DeploymentStatus_SUCCESS {
		t.Fatalf("deployment failed: %s", response.GetState().GetMessage())
	}

	output := response.GetDeployment().GetKubernetes()
	if output.GetProfile() != builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1 {
		t.Fatalf("output profile = %s", output.GetProfile())
	}
	if output.GetContractVersion() != services.KubernetesManifestContractVersion {
		t.Fatalf("contract version = %q", output.GetContractVersion())
	}
	if output.GetValidation().GetStaticValidation() != builderv0.KubernetesManifestValidation_STATUS_PASSED {
		t.Fatalf("static validation failed: %v", output.GetValidation().GetViolations())
	}

	assertRestrictedManifestBundle(t, output, accessKeyEnv, secretKeyEnv)

	configuration := response.GetConfiguration()
	if configuration.GetOrigin() != builder.Unique() {
		t.Fatalf("configuration origin = %q, want %q", configuration.GetOrigin(), builder.Unique())
	}
	infos := configuration.GetInfos()
	if len(infos) != 1 || infos[0].GetName() != "minio" {
		t.Fatalf("connection configuration infos = %v", infos)
	}
	for _, value := range infos[0].GetConfigurationValues() {
		if value.GetKey() == "endpoint" {
			continue
		}
		if !value.GetSecret() || value.GetValue() != "" {
			t.Fatalf("credential descriptor %q = %+v, want a value-free secret reference", value.GetKey(), value)
		}
	}

	statefulSet := readDeploymentFile(t, destination, "base", "stateful-set.yaml")
	for _, expected := range []string{
		"automountServiceAccountToken: false",
		"image: " + image.FullName(),
		"name: MINIO_ACCESS_KEY",
		"name: MINIO_SECRET_KEY",
		"name: minio-credentials",
		"key: access-key",
		"key: secret-key",
	} {
		if !strings.Contains(statefulSet, expected) {
			t.Errorf("restricted StatefulSet missing %q:\n%s", expected, statefulSet)
		}
	}
	for _, unexpected := range []string{
		"secretRef:",
		accessKeyEnv,
		secretKeyEnv,
		"UNRELATED_SECRET",
		"unrelated-credentials",
	} {
		if strings.Contains(statefulSet, unexpected) {
			t.Errorf("restricted StatefulSet contains %q:\n%s", unexpected, statefulSet)
		}
	}

	tree := readManifestTree(t, destination)
	for _, unexpected := range []string{"kind: Namespace", "kind: Secret", "\nstringData:"} {
		if strings.Contains(tree, unexpected) {
			t.Errorf("restricted manifest tree contains %q:\n%s", unexpected, tree)
		}
	}
}

func TestRestrictedPortableRejectsMissingSecretReferences(t *testing.T) {
	builder, networkMappings := newDeploymentTestBuilder(t)

	response, err := builder.Deploy(context.Background(), restrictedDeploymentRequest(
		t.TempDir(),
		networkMappings,
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetState().GetState() != builderv0.DeploymentStatus_ERROR {
		t.Fatalf("deployment status = %s, want ERROR", response.GetState().GetState())
	}
	if !strings.Contains(response.GetState().GetMessage(), "requires typed Kubernetes Secret references") {
		t.Fatalf("deployment error = %q", response.GetState().GetMessage())
	}
	if response.GetConfiguration() != nil {
		t.Fatalf("failed deployment returned configuration: %+v", response.GetConfiguration())
	}
}

func newDeploymentTestBuilder(t *testing.T) (*Builder, []*basev0.NetworkMapping) {
	t.Helper()
	ctx := context.Background()
	builder := NewBuilder()
	identity := &basev0.ServiceIdentity{
		Workspace: "workspace",
		Module:    "module",
		Name:      "minio",
		Version:   "1.2.3",
	}
	if err := builder.HeadlessLoad(ctx, identity); err != nil {
		t.Fatal(err)
	}
	builder.Information = &services.Information{
		Service: resources.ToServiceWithCase(builder.Identity),
		Module:  resources.ToModuleWithCase(builder.Identity),
	}
	builder.EnvironmentVariables.SetIdentity(identity)
	builder.TcpEndpoint = &basev0.Endpoint{
		Name:    "tcp",
		Module:  identity.Module,
		Service: identity.Name,
		Api:     "tcp",
	}
	instance := resources.NewNetworkInstance("minio.example.com", 9000)
	instance.Access = resources.NewContainerNetworkAccess()
	return builder, []*basev0.NetworkMapping{{
		Endpoint:  builder.TcpEndpoint,
		Instances: []*basev0.NetworkInstance{instance},
	}}
}

func restrictedDeploymentRequest(
	destination string,
	networkMappings []*basev0.NetworkMapping,
	secretReferences map[string]*builderv0.KubernetesSecretKeyReference,
) *builderv0.DeploymentRequest {
	return &builderv0.DeploymentRequest{
		Environment:     &basev0.Environment{Name: "test"},
		NetworkMappings: networkMappings,
		Deployment: &builderv0.Deployment{Kind: &builderv0.Deployment_Kubernetes{
			Kubernetes: &builderv0.KubernetesDeployment{
				Namespace:        "codefly-test",
				Destination:      destination,
				Profile:          builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1,
				SecretReferences: secretReferences,
			},
		}},
	}
}

func readDeploymentFile(t *testing.T, directory string, elements ...string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(append([]string{directory}, elements...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func readManifestTree(t *testing.T, root string) string {
	t.Helper()
	var manifests strings.Builder
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		manifests.Write(content)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return manifests.String()
}

func assertRestrictedManifestBundle(t *testing.T, output *builderv0.KubernetesDeploymentOutput, secretKeys ...string) {
	t.Helper()
	bundle := output.GetBundle()
	if bundle == nil {
		t.Fatal("restricted output carries no manifest bundle")
	}
	if bundle.GetProfile() != builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1 {
		t.Errorf("bundle profile = %s", bundle.GetProfile())
	}
	if bundle.GetContractVersion() != services.KubernetesManifestContractVersion {
		t.Errorf("bundle contract version = %q", bundle.GetContractVersion())
	}
	if len(bundle.GetEntryPoints()) == 0 {
		t.Error("bundle exposes no entry points")
	}
	if !strings.HasPrefix(bundle.GetDigest(), "sha256:") {
		t.Errorf("bundle digest = %q, want sha256-pinned aggregate", bundle.GetDigest())
	}
	if len(bundle.GetFiles()) == 0 {
		t.Fatal("bundle inventory is empty")
	}
	for _, file := range bundle.GetFiles() {
		if file.GetPath() == "" || !strings.HasPrefix(file.GetDigest(), "sha256:") {
			t.Errorf("bundle inventory entry = %+v, want path and sha256 digest", file)
		}
	}
	if bundle.GetValidation().GetStaticValidation() != builderv0.KubernetesManifestValidation_STATUS_PASSED {
		t.Errorf("bundle validation not passed: %v", bundle.GetValidation().GetViolations())
	}
	for _, key := range secretKeys {
		reference := bundle.GetSecretReferences()[key]
		if reference.GetName() == "" || reference.GetKey() == "" {
			t.Errorf("bundle dropped the external Secret reference for %s", key)
		}
	}
}
