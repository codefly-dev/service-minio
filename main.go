package main

import (
	"context"
	"embed"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/builders"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	runnersbase "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/templates"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Agent version
var agent = shared.Must(resources.LoadFromFs[resources.Agent](shared.Embed(infoFS)))

var requirements = builders.NewDependencies(agent.Name, builders.NewDependency("service.codefly.yaml"))

type Settings struct {
}

const HotReload = "hot-reload"
const DatabaseName = "database-name"

var image = &resources.DockerImage{
	Name:   "minio/minio",
	Tag:    "RELEASE.2025-09-07T16-13-09Z",
	Digest: "sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e",
}

type Service struct {
	*services.Base

	// Settings
	*Settings

	accessKey string
	secretKey string

	TcpEndpoint *basev0.Endpoint
}

func (s *Service) GetAgentInformation(ctx context.Context, _ *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {

	readme, err := templates.ApplyTemplateFrom(ctx, shared.Embed(readmeFS), "templates/agent/README.md", s.Information)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return services.Advertisement{
		Backends: runnersbase.BackendSupport{
			Docker: true,
		},
		Config: []*agentv0.ConfigurationValueDetail{
			{
				Name: "minio", Description: "minio credentials",
				Fields: []*agentv0.ConfigurationValueInformation{
					{
						Name: "access-key", Description: "access key",
					}, {
						Name: "secret-key", Description: "secret key",
					},
				},
			},
		},
		ReadMe: readme,
	}.Build(), nil
}

func NewService() *Service {
	return &Service{
		Base:     services.NewServiceBase(context.Background(), agent.Of(resources.ServiceAgent)),
		Settings: &Settings{},
	}
}

func (s *Service) LoadConfiguration(ctx context.Context, conf *basev0.Configuration) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	var err error
	s.accessKey, err = resources.GetConfigurationValue(ctx, conf, "minio", "MINIO_ACCESS_KEY")
	if err != nil {
		return s.Wool.Wrapf(err, "cannot get access key")
	}
	s.secretKey, err = resources.GetConfigurationValue(ctx, conf, "minio", "MINIO_SECRET_KEY")
	if err != nil {
		return s.Wool.Wrapf(err, "cannot get secret key")
	}
	return nil
}

func (s *Service) CreateCredentialsConfiguration(ctx context.Context, conf *basev0.Configuration, instance *basev0.NetworkInstance) (*basev0.Configuration, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)
	outputConf := &basev0.Configuration{
		Origin:         s.Base.Unique(),
		RuntimeContext: resources.RuntimeContextFromInstance(instance),
		Infos: []*basev0.ConfigurationInformation{
			{Name: "minio",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "endpoint", Value: instance.Address},
					{Key: "access-key", Value: s.accessKey, Secret: true},
					{Key: "secret-key", Value: s.secretKey, Secret: true},
				},
			},
		},
	}
	return outputConf, nil
}

func main() {
	agents.Serve(agents.PluginRegistration{
		Agent:   NewService(),
		Runtime: NewRuntime(),
		Builder: NewBuilder(),
	})
}

//go:embed agent.codefly.yaml
var infoFS embed.FS

//go:embed templates/agent
var readmeFS embed.FS
