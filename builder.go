package main

import (
	"context"
	"embed"
	"fmt"

	v0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/wool"

	"github.com/codefly-dev/core/agents/communicate"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"

	"github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

type Builder struct {
	services.BuilderServer
	*Service
}

// deploymentTemplateParameters carries the plugin-specific values a restricted
// render needs: identifier-only references to the externally managed Secret
// keys that hold MinIO's root credentials. It never carries secret values.
type deploymentTemplateParameters struct {
	AccessKeyReference *builderv0.KubernetesSecretKeyReference
	SecretKeyReference *builderv0.KubernetesSecretKeyReference
}

func NewBuilder() *Builder {
	return &Builder{
		Service: NewService(),
	}
}

func (s *Builder) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	defer s.Wool.Catch()

	return s.Builder.LoadService(ctx, req, services.BuilderLoad{
		Settings:         s.Settings,
		Requirements:     requirements,
		FactoryTemplates: factoryFS,
		ResolveEndpoints: func(ctx context.Context, endpoints []*v0.Endpoint) error {
			endpoint, err := resources.FindTCPEndpoint(ctx, endpoints)
			if err != nil {
				return err
			}
			s.TcpEndpoint = endpoint
			s.Wool.Debug("endpoint", wool.Field("tcp", endpoint))
			return nil
		},
	})
}

func (s *Builder) Init(ctx context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	defer s.Wool.Catch()

	return s.Builder.InitResponse()
}

func (s *Builder) Update(ctx context.Context, req *builderv0.UpdateRequest) (*builderv0.UpdateResponse, error) {
	defer s.Wool.Catch()

	return &builderv0.UpdateResponse{}, nil
}

func (s *Builder) Sync(ctx context.Context, req *builderv0.SyncRequest) (*builderv0.SyncResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	return s.Builder.SyncResponse()
}

func (s *Builder) Audit(ctx context.Context, req *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.AuditContainer(ctx, req, image.FullName())
}

func (s *Builder) SBOM(ctx context.Context, _ *builderv0.SBOMRequest) (*builderv0.SBOMResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	return s.Builder.SBOMContainer(ctx, image.FullName())
}

func (s *Builder) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	defer s.Wool.Catch()

	return s.Builder.BuildResponse()
}

func (s *Builder) Deploy(ctx context.Context, req *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	defer s.Wool.Catch()
	s.Base.SetDockerImage(image)

	parameters := &deploymentTemplateParameters{}
	var restrictedConfiguration *v0.Configuration
	response, err := s.Builder.DeployKustomize(ctx, req, services.KustomizeDeployment{
		EnvironmentVariables: s.EnvironmentVariables,
		Templates:            deploymentFS,
		Parameters:           parameters,
		Prepare: func(ctx context.Context, deployment *services.KustomizeDeploymentContext) error {
			configuration, prepareErr := s.prepareDeployment(ctx, deployment, parameters)
			if prepareErr != nil {
				return prepareErr
			}
			// A restricted render must not receive secret values, so the
			// connection configuration is returned as a value-free reference
			// on the response instead of being exported into the manifests.
			if services.IsRestrictedOutputProfile(deployment.Profile) {
				restrictedConfiguration = configuration
				return nil
			}
			return deployment.ExportConfiguration(ctx, configuration)
		},
	})
	if err != nil ||
		response.GetState().GetState() != builderv0.DeploymentStatus_SUCCESS ||
		restrictedConfiguration == nil {
		return response, err
	}
	response.Configuration = restrictedConfiguration
	return response, nil
}

func (s *Builder) prepareDeployment(
	ctx context.Context,
	deployment *services.KustomizeDeploymentContext,
	parameters *deploymentTemplateParameters,
) (*v0.Configuration, error) {
	req := deployment.Request
	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, req.GetNetworkMappings(), s.TcpEndpoint, resources.NewContainerNetworkAccess())
	if err != nil {
		return nil, err
	}
	if services.IsRestrictedOutputProfile(deployment.Profile) {
		accessKeyEnv := resources.ServiceSecretConfigurationKeyFromUnique(s.Unique(), "minio", "MINIO_ACCESS_KEY")
		secretKeyEnv := resources.ServiceSecretConfigurationKeyFromUnique(s.Unique(), "minio", "MINIO_SECRET_KEY")
		references := deployment.Kubernetes.GetSecretReferences()
		accessKeyReference := references[accessKeyEnv]
		secretKeyReference := references[secretKeyEnv]
		if accessKeyReference == nil || secretKeyReference == nil {
			return nil, fmt.Errorf("minio requires typed Kubernetes Secret references for %s and %s", accessKeyEnv, secretKeyEnv)
		}
		if accessKeyReference.GetOptional() || secretKeyReference.GetOptional() {
			return nil, fmt.Errorf("minio Secret references must not be optional")
		}
		parameters.AccessKeyReference = accessKeyReference
		parameters.SecretKeyReference = secretKeyReference
		return s.restrictedCredentialsConfiguration(instance), nil
	}
	if err = s.LoadConfiguration(ctx, req.GetConfiguration()); err != nil {
		return nil, err
	}
	deployment.AddSecrets(
		resources.Env("MINIO_ACCESS_KEY", s.accessKey),
		resources.Env("MINIO_SECRET_KEY", s.secretKey),
	)
	return s.CreateCredentialsConfiguration(ctx, req.GetConfiguration(), instance)
}

func (s *Builder) Options() []*agentv0.Question {
	return []*agentv0.Question{}
}

func (s *Builder) Communicate(stream builderv0.Builder_CommunicateServer) error {
	asker := communicate.NewQuestionAsker(stream)
	_, err := asker.RunSequence(s.Options())
	return err
}

type create struct {
}

func (s *Builder) Create(ctx context.Context, req *builderv0.CreateRequest) (*builderv0.CreateResponse, error) {
	defer s.Wool.Catch()

	c := create{}

	err := s.Templates(ctx, c, services.WithFactory(factoryFS))
	if err != nil {
		return s.Builder.CreateError(err)
	}

	err = s.CreateEndpoints(ctx)
	if err != nil {
		return s.Builder.CreateErrorf(err, "cannot create endpoints")
	}

	s.Wool.Debug("created endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(s.Endpoints)))

	return s.Builder.CreateResponse(ctx, s.Settings)
}

func (s *Builder) CreateEndpoints(ctx context.Context) error {
	tcp, err := resources.LoadTCPAPI(ctx)
	if err != nil {
		return s.Wool.Wrapf(err, "cannot load tcp api")
	}
	endpoint := s.Base.BaseEndpoint(standards.TCP)
	s.TcpEndpoint, err = resources.NewAPI(ctx, endpoint, resources.ToTCPAPI(tcp))
	s.Endpoints = []*v0.Endpoint{s.TcpEndpoint}
	return nil
}

//go:embed templates/factory
var factoryFS embed.FS

//go:embed templates/builder
var builderFS embed.FS

//go:embed templates/deployment
var deploymentFS embed.FS
