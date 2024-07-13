package main

import (
	"context"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"os"
	"time"

	"github.com/codefly-dev/core/agents/helpers/code"

	"github.com/codefly-dev/core/wool"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/base"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
)

type Runtime struct {
	*Service

	// internal
	runnerEnvironment *runners.DockerEnvironment

	minioPort uint16

	// For ready check
	hostReady string
}

func NewRuntime() *Runtime {
	return &Runtime{
		Service: NewService(),
	}
}

func (s *Runtime) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	err := s.Base.Load(ctx, req.Identity, s.Settings)
	if err != nil {
		return s.Runtime.LoadError(err)
	}

	s.Runtime.SetEnvironment(req.Environment)

	requirements.Localize(s.Location)

	// Endpoints
	s.Endpoints, err = s.Runtime.Service.LoadEndpoints(ctx)
	if err != nil {
		return s.Runtime.LoadError(err)
	}

	s.Wool.Debug("endpoints", wool.Field("endpoints", resources.MakeManyEndpointSummary(s.Endpoints)))

	s.TcpEndpoint, err = resources.FindTCPEndpoint(ctx, s.Endpoints)
	if err != nil {
		return s.Runtime.LoadError(err)
	}

	return s.Runtime.LoadResponse()
}

func CallingContext() *basev0.NetworkAccess {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return resources.NewContainerNetworkAccess()
	}
	return resources.NewNativeNetworkAccess()
}

func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Runtime.LogInitRequest(req)

	w := s.Wool.In("runtime::init")

	s.NetworkMappings = req.ProposedNetworkMappings

	s.Configuration = req.Configuration

	err := s.LoadConfiguration(ctx, s.Configuration)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	net, err := resources.FindNetworkMapping(ctx, s.NetworkMappings, s.TcpEndpoint)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if net == nil {
		return s.Runtime.InitError(w.NewError("network mapping is nil"))
	}

	instance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.TcpEndpoint, CallingContext())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	if instance == nil {
		return s.Runtime.InitError(w.NewError("network instance is nil"))
	}

	w.Debug("tcp network instance", wool.Field("instance", instance))

	s.Infof("will run on %s", instance.Host)
	s.minioPort = 9000

	// Create configuration
	for _, inst := range net.Instances {
		conf, errConn := s.CreateCredentialsConfiguration(ctx, s.Configuration, inst)
		if errConn != nil {
			return s.Runtime.InitError(errConn)
		}
		w.Debug("adding configuration", wool.Field("config", resources.MakeConfigurationSummary(conf)), wool.Field("instance", inst))
		s.Runtime.RuntimeConfigurations = append(s.Runtime.RuntimeConfigurations, conf)
	}
	s.Wool.Focus("sending runtime configuration", wool.Field("conf", resources.MakeManyConfigurationSummary(s.Runtime.RuntimeConfigurations)))

	w.Debug("setting up connection string for migrations")

	// Setup a connection string for migration
	hostInstance, err := resources.FindNetworkInstanceInNetworkMappings(ctx, s.NetworkMappings, s.TcpEndpoint, CallingContext())
	if err != nil {
		return s.Runtime.InitError(err)
	}
	s.hostReady = hostInstance.Host

	// Docker
	runner, err := runners.NewDockerHeadlessEnvironment(ctx, image, s.UniqueWithWorkspace())
	if err != nil {
		return s.Runtime.InitError(err)
	}

	runner.WithOutput(s.Wool)
	runner.WithCommand("server", "/data")
	runner.WithPortMapping(ctx, uint16(instance.Port), s.minioPort)

	runner.WithEnvironmentVariables(
		ctx,
		resources.Env("MINIO_ACCESS_KEY", s.accessKey),
		resources.Env("MINIO_SECRET_KEY", s.secretKey),
	)

	s.runnerEnvironment = runner

	w.Debug("init for runner environment: will start container")
	err = s.runnerEnvironment.Init(ctx)
	if err != nil {
		return s.Runtime.InitError(err)
	}

	s.Wool.Debug("init successful")
	return s.Runtime.InitResponse()
}

func (s *Runtime) WaitForReady(ctx context.Context) error {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Focus("waiting for ready")

	minioClient, err := minio.New(s.hostReady, &minio.Options{
		Creds: credentials.NewStaticV4(s.accessKey, s.secretKey, ""),
	})
	if err != nil {
		return s.Wool.Wrapf(err, "cannot create minio client")
	}

	// Set a timeout for the operation

	maxRetry := 5
	for retry := 0; retry < maxRetry; retry++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// List buckets
		_, err = minioClient.ListBuckets(ctx)
		if err == nil {
			return nil
		}
	}
	return s.Wool.NewError("database is not ready")
}

func (s *Runtime) Start(ctx context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("starting")

	s.Wool.Debug("waiting for ready")

	err := s.WaitForReady(ctx)
	if err != nil {
		return s.Runtime.StartError(err)
	}

	s.Wool.Debug("start done")
	return s.Runtime.StartResponse()
}

func (s *Runtime) Information(ctx context.Context, req *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	return s.Runtime.InformationResponse(ctx, req)
}

func (s *Runtime) Stop(ctx context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	defer s.Wool.Catch()

	s.Wool.Debug("nothing to stop: keep environment alive")

	err := s.Base.Stop()
	if err != nil {
		return s.Runtime.StopError(err)
	}
	return s.Runtime.StopResponse()
}

func (s *Runtime) Destroy(ctx context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	defer s.Wool.Catch()
	ctx = s.Wool.Inject(ctx)

	s.Wool.Debug("Destroying")

	// Get the runner environment
	runner, err := runners.NewDockerHeadlessEnvironment(ctx, image, s.UniqueWithWorkspace())
	if err != nil {
		return s.Runtime.DestroyError(err)
	}

	err = runner.Shutdown(ctx)
	if err != nil {
		return s.Runtime.DestroyError(err)
	}
	return s.Runtime.DestroyResponse()
}

func (s *Runtime) Test(ctx context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	return s.Runtime.TestResponse()
}

func (s *Runtime) Communicate(ctx context.Context, req *agentv0.Engage) (*agentv0.InformationRequest, error) {
	return s.Base.Communicate(ctx, req)
}

/* Details

 */

func (s *Runtime) EventHandler(event code.Change) error {
	return nil
}
