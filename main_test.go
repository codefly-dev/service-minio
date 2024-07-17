package main

import (
	"context"
	"fmt"
	"github.com/codefly-dev/core/agents"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"os"
	"path"
	"testing"
	"time"
)

func TestCreateToRun(t *testing.T) {
	wool.SetGlobalLogLevel(wool.DEBUG)
	agents.LogToConsole()
	ctx := context.Background()

	workspace := &resources.Workspace{Name: "test"}

	tmpDir := t.TempDir()
	defer func(path string) {
		err := os.RemoveAll(path)
		require.NoError(t, err)
	}(tmpDir)

	serviceName := fmt.Sprintf("svc-%v", time.Now().UnixMilli())
	service := resources.Service{Name: serviceName, Version: "test-me"}
	service.WithModule("mod")
	err := service.SaveAtDir(ctx, path.Join(tmpDir, "mod", service.Name))

	require.NoError(t, err)

	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Module:              "mod",
		Workspace:           workspace.Name,
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: fmt.Sprintf("mod/%s", service.Name),
	}
	builder := NewBuilder()

	resp, err := builder.Load(ctx, &builderv0.LoadRequest{DisableCatch: true, Identity: identity, CreationMode: &builderv0.CreationMode{Communicate: false}})
	require.NoError(t, err)
	require.NotNil(t, resp)

	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	// Now run it
	runtime := NewRuntime()

	// Create temporary network mappings
	networkManager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	networkManager.WithTemporaryPorts()

	env := resources.LocalEnvironment()

	_, err = runtime.Load(ctx, &runtimev0.LoadRequest{
		Identity:     identity,
		Environment:  shared.Must(env.Proto()),
		DisableCatch: true})
	require.NoError(t, err)

	require.Equal(t, 1, len(runtime.Endpoints))

	networkMappings, err := networkManager.GenerateNetworkMappings(ctx, env, workspace, runtime.Identity, runtime.Endpoints)
	require.NoError(t, err)
	require.Equal(t, 1, len(networkMappings))

	// Configurations are passed in
	conf := &basev0.Configuration{
		Origin: fmt.Sprintf("mod/%s", service.Name),

		RuntimeContext: resources.NewRuntimeContextFree(),
		Infos: []*basev0.ConfigurationInformation{
			{Name: "minio",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "MINIO_ACCESS_KEY", Value: "minio"},
					{Key: "MINIO_SECRET_KEY", Value: "password"},
				},
			},
		},
	}

	init, err := runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          resources.NewRuntimeContextFree(),
		Configuration:           conf,
		ProposedNetworkMappings: networkMappings,
	})
	require.NoError(t, err)
	require.NotNil(t, init)

	defer func() {
		_, err = runtime.Destroy(ctx, &runtimev0.DestroyRequest{})
	}()

	// Extract logs

	_, err = runtime.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)

	// Get the configuration and connect to minio
	configurationOut, err := resources.ExtractConfiguration(init.RuntimeConfigurations, resources.NewRuntimeContextNative())
	require.NoError(t, err)

	endpoint, err := resources.GetConfigurationValue(ctx, configurationOut, "minio", "endpoint")
	require.NoError(t, err)

	accessKeyID, err := resources.GetConfigurationValue(ctx, configurationOut, "minio", "access-key")
	require.NoError(t, err)

	secretKeyID, err := resources.GetConfigurationValue(ctx, configurationOut, "minio", "secret-key")
	require.NoError(t, err)

	// Initialize minio client object.
	minioClient, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(accessKeyID, secretKeyID, "")})
	require.NoError(t, err)

	// Create a bucket
	bucketName := "testbucket"
	location := "us-east-1"

	err = minioClient.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{Region: location})
	if err != nil {
		// Check to see if we already own this bucket (which happens if you run this twice)
		exists, errBucketExists := minioClient.BucketExists(ctx, bucketName)
		if errBucketExists == nil && !exists {
			t.Fatal(err)
		}
	}

	// Upload the test file
	// Change the value of filePath if the file is in another location
	objectName := "test.text"
	filePath := shared.MustSolvePath("testdata/test.txt")
	contentType := "application/octet-stream"

	// Upload the test file with FPutObject
	info, err := minioClient.FPutObject(ctx, bucketName, objectName, filePath, minio.PutObjectOptions{ContentType: contentType})
	require.NoError(t, err)

	// Verify the upload
	assert.NotZero(t, info.Size, "Uploaded file size should not be zero")
	assert.Equal(t, objectName, info.Key, "Object name should match")

	// Check if the file exists in the bucket
	_, err = minioClient.StatObject(ctx, bucketName, objectName, minio.StatObjectOptions{})
	assert.NoError(t, err, "Object should exist in the bucket")

	// Download the file and compare contents
	tmpFile, err := os.CreateTemp("", "minio-test-*")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	err = minioClient.FGetObject(ctx, bucketName, objectName, tmpFile.Name(), minio.GetObjectOptions{})
	require.NoError(t, err)

	originalContent, err := os.ReadFile(filePath)
	require.NoError(t, err)

	downloadedContent, err := os.ReadFile(tmpFile.Name())
	require.NoError(t, err)

	assert.Equal(t, originalContent, downloadedContent, "Downloaded content should match original content")

	// Clean up: remove the object and bucket
	err = minioClient.RemoveObject(ctx, bucketName, objectName, minio.RemoveObjectOptions{})
	require.NoError(t, err)

	err = minioClient.RemoveBucket(ctx, bucketName)
	require.NoError(t, err)
}
