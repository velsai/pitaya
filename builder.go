package pitaya

import (
	"github.com/go-redis/redis/v8"
	"github.com/topfreegames/pitaya/v2/acceptor"
	"github.com/topfreegames/pitaya/v2/agent"
	"github.com/topfreegames/pitaya/v2/cluster"
	"github.com/topfreegames/pitaya/v2/config"
	confPkg "github.com/topfreegames/pitaya/v2/config"
	"github.com/topfreegames/pitaya/v2/conn/codec"
	"github.com/topfreegames/pitaya/v2/conn/message"
	"github.com/topfreegames/pitaya/v2/defaultpipelines"
	"github.com/topfreegames/pitaya/v2/groups"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/metrics"
	"github.com/topfreegames/pitaya/v2/metrics/models"
	"github.com/topfreegames/pitaya/v2/pipeline"
	"github.com/topfreegames/pitaya/v2/router"
	"github.com/topfreegames/pitaya/v2/serialize"
	"github.com/topfreegames/pitaya/v2/serialize/json"
	"github.com/topfreegames/pitaya/v2/service"
	"github.com/topfreegames/pitaya/v2/session"
	"github.com/topfreegames/pitaya/v2/util"
	"github.com/topfreegames/pitaya/v2/worker"
	"go.uber.org/zap"
)

// Builder holds dependency instances for a pitaya App
type Builder struct {
	acceptors        []acceptor.Acceptor
	Config           config.BuilderConfig
	DieChan          chan bool
	PacketDecoder    codec.PacketDecoder
	PacketEncoder    codec.PacketEncoder
	MessageEncoder   *message.MessagesEncoder
	Serializer       serialize.Serializer
	Router           *router.Router
	RPCClient        cluster.RPCClient
	RPCServer        cluster.RPCServer
	MetricsReporters []metrics.Reporter
	Server           *cluster.Server
	ServerMode       ServerMode
	ServiceDiscovery cluster.ServiceDiscovery
	Groups           groups.GroupService
	SessionPool      session.SessionPool
	Worker           *worker.Worker
	HandlerHooks     *pipeline.HandlerHooks
	RemoteHooks      *pipeline.HandlerHooks
	Redis            redis.Cmdable
	conf             *config.Config
}

// PitayaBuilder Builder interface
type PitayaBuilder interface {
	Build() Pitaya
}

// NewBuilderWithConfigs return a builder instance with default dependency instances for a pitaya App
// with configs defined by a config file (config.Config) and default paths (see documentation).
func NewBuilderWithConfigs(
	isFrontend bool,
	serverType string,
	serverMode ServerMode,
	serverMetadata map[string]string,
	conf *config.Config,
) *Builder {
	builderConfig := config.NewBuilderConfig(conf)
	customMetrics := config.NewCustomMetricsSpec(conf)
	prometheusConfig := config.NewPrometheusConfig(conf)
	statsdConfig := config.NewStatsdConfig(conf)
	etcdSDConfig := config.NewEtcdServiceDiscoveryConfig(conf)
	natsRPCServerConfig := config.NewNatsRPCServerConfig(conf)
	natsRPCClientConfig := config.NewNatsRPCClientConfig(conf)
	workerConfig := config.NewWorkerConfig(conf)
	enqueueOpts := config.NewEnqueueOpts(conf)
	groupServiceConfig := config.NewMemoryGroupConfig(conf)
	redisConfig := config.NewRedisConfig(conf)
	conf.AddLoader(logger.Manager.ReloadFactory("pitaya.log", func() {
		logger.Zap = logger.Manager.Log
		logger.Sugar = logger.Manager.Sugar
		logger.Log = logger.Sugar
	}))
	b := NewBuilder(
		isFrontend,
		serverType,
		serverMode,
		serverMetadata,
		*builderConfig,
		*customMetrics,
		*prometheusConfig,
		*statsdConfig,
		*etcdSDConfig,
		*natsRPCServerConfig,
		*natsRPCClientConfig,
		*workerConfig,
		*enqueueOpts,
		*groupServiceConfig,
		*redisConfig,
	)
	b.conf = conf
	return b
}

// NewDefaultBuilder return a builder instance with default dependency instances for a pitaya App,
// with default configs
func NewDefaultBuilder(isFrontend bool, serverType string, serverMode ServerMode, serverMetadata map[string]string, builderConfig config.BuilderConfig) *Builder {
	customMetrics := config.NewDefaultCustomMetricsSpec()
	prometheusConfig := config.NewDefaultPrometheusConfig()
	statsdConfig := config.NewDefaultStatsdConfig()
	etcdSDConfig := config.NewDefaultEtcdServiceDiscoveryConfig()
	natsRPCServerConfig := config.NewDefaultNatsRPCServerConfig()
	natsRPCClientConfig := config.NewDefaultNatsRPCClientConfig()
	workerConfig := config.NewDefaultWorkerConfig()
	enqueueOpts := config.NewDefaultEnqueueOpts()
	groupServiceConfig := config.NewDefaultMemoryGroupConfig()
	redisConfig := config.NewDefaultRedisConfig()
	return NewBuilder(
		isFrontend,
		serverType,
		serverMode,
		serverMetadata,
		builderConfig,
		*customMetrics,
		*prometheusConfig,
		*statsdConfig,
		*etcdSDConfig,
		*natsRPCServerConfig,
		*natsRPCClientConfig,
		*workerConfig,
		*enqueueOpts,
		*groupServiceConfig,
		*redisConfig,
	)
}

// NewBuilder return a builder instance with default dependency instances for a pitaya App,
// with configs explicitly defined
func NewBuilder(isFrontend bool,
	serverType string,
	serverMode ServerMode,
	serverMetadata map[string]string,
	config config.BuilderConfig,
	customMetrics models.CustomMetricsSpec,
	prometheusConfig config.PrometheusConfig,
	statsdConfig config.StatsdConfig,
	etcdSDConfig config.EtcdServiceDiscoveryConfig,
	natsRPCServerConfig config.NatsRPCServerConfig,
	natsRPCClientConfig config.NatsRPCClientConfig,
	workerConfig config.WorkerConfig,
	enqueueOpts config.EnqueueOpts,
	groupServiceConfig config.MemoryGroupConfig,
	redisConfig config.RedisConfig,
) *Builder {
	server := cluster.NewServer(util.NanoID(8), serverType, isFrontend, serverMetadata)
	dieChan := make(chan bool)

	metricsReporters := []metrics.Reporter{}
	if config.Metrics.Prometheus.Enabled {
		metricsReporters = addDefaultPrometheus(prometheusConfig, customMetrics, metricsReporters, serverType)
	}

	if config.Metrics.Statsd.Enabled {
		metricsReporters = addDefaultStatsd(statsdConfig, metricsReporters, serverType)
	}

	handlerHooks := pipeline.NewHandlerHooks()
	if config.DefaultPipelines.StructValidation.Enabled {
		configureDefaultPipelines(handlerHooks)
	}
	remoteHooks := pipeline.NewHandlerHooks()
	if config.DefaultPipelines.StructValidation.Enabled {
		configureDefaultPipelines(remoteHooks)
	}

	// session 后端redis落地实例
	var redisClient redis.Cmdable
	if redisConfig.Type == "cluster" {
		redisClient = redis.NewClusterClient(confPkg.ToRedisClusterOption(&redisConfig))
	} else {
		redisClient = redis.NewClient(confPkg.ToRedisNodeConfig(&redisConfig))
	}
	sessionCache := session.NewRedisCache(redisClient, config.Pitaya.Session.CacheTTL)
	sessionPool := session.NewSessionPool()
	sessionPool.SetClusterCache(sessionCache)

	var serviceDiscovery cluster.ServiceDiscovery
	var rpcServer cluster.RPCServer
	var rpcClient cluster.RPCClient
	if serverMode == Cluster {
		var err error
		serviceDiscovery, err = cluster.NewEtcdServiceDiscovery(etcdSDConfig, server, dieChan)
		if err != nil {
			logger.Zap.Fatal("error creating default cluster service discovery component", zap.Error(err))
		}

		rpcServer, err = cluster.NewNatsRPCServer(natsRPCServerConfig, server, metricsReporters, dieChan, sessionPool)
		if err != nil {
			logger.Zap.Fatal("error setting default cluster rpc server component", zap.Error(err))
		}

		rpcClient, err = cluster.NewNatsRPCClient(natsRPCClientConfig, server, metricsReporters, dieChan)
		if err != nil {
			logger.Zap.Fatal("error setting default cluster rpc client component", zap.Error(err))
		}
	}

	worker, err := worker.NewWorker(workerConfig, enqueueOpts)
	if err != nil {
		logger.Zap.Fatal("error creating default worker", zap.Error(err))
	}

	gsi := groups.NewMemoryGroupService(groupServiceConfig)
	if err != nil {
		panic(err)
	}

	return &Builder{
		acceptors:        []acceptor.Acceptor{},
		Config:           config,
		DieChan:          dieChan,
		PacketDecoder:    codec.NewPomeloPacketDecoder(),
		PacketEncoder:    codec.NewPomeloPacketEncoder(),
		MessageEncoder:   message.NewMessagesEncoder(config.Pitaya.Handler.Messages.Compression),
		Serializer:       json.NewSerializer(),
		Router:           router.New(),
		RPCClient:        rpcClient,
		RPCServer:        rpcServer,
		MetricsReporters: metricsReporters,
		Server:           server,
		ServerMode:       serverMode,
		Groups:           gsi,
		HandlerHooks:     handlerHooks,
		RemoteHooks:      remoteHooks,
		ServiceDiscovery: serviceDiscovery,
		SessionPool:      sessionPool,
		Worker:           worker,
		Redis:            redisClient,
	}
}

// AddAcceptor adds a new acceptor to app
func (builder *Builder) AddAcceptor(ac acceptor.Acceptor) {
	if !builder.Server.Frontend {
		logger.Zap.Error("tried to add an acceptor to a backend server, skipping")
		return
	}
	builder.acceptors = append(builder.acceptors, ac)
}

// Build returns a valid App instance
func (builder *Builder) Build() Pitaya {
	handlerPool := service.NewHandlerPool()
	var remoteService *service.RemoteService
	if builder.ServerMode == Standalone {
		if builder.ServiceDiscovery != nil || builder.RPCClient != nil || builder.RPCServer != nil {
			panic("Standalone mode can't have RPC or service discovery instances")
		}
	} else {
		if !(builder.ServiceDiscovery != nil && builder.RPCClient != nil && builder.RPCServer != nil) {
			panic("Cluster mode must have RPC and service discovery instances")
		}

		builder.Router.SetServiceDiscovery(builder.ServiceDiscovery)

		remoteService = service.NewRemoteService(
			builder.RPCClient,
			builder.RPCServer,
			builder.ServiceDiscovery,
			builder.PacketEncoder,
			builder.Serializer,
			builder.Router,
			builder.MessageEncoder,
			builder.Server,
			builder.SessionPool,
			builder.RemoteHooks,
			builder.HandlerHooks,
			handlerPool,
		)

		builder.RPCServer.SetPitayaServer(remoteService)
	}

	agentFactory := agent.NewAgentFactory(builder.DieChan,
		builder.PacketDecoder,
		builder.PacketEncoder,
		builder.Serializer,
		builder.Config.Pitaya.Heartbeat.Interval,
		builder.MessageEncoder,
		builder.Config.Pitaya.Buffer.Agent.Messages,
		builder.SessionPool,
		builder.MetricsReporters,
		builder.Server.ID,
	)

	handlerService := service.NewHandlerService(
		builder.PacketDecoder,
		builder.Serializer,
		builder.Config.Pitaya.Buffer.Handler.LocalProcess,
		builder.Config.Pitaya.Buffer.Handler.RemoteProcess,
		builder.Server,
		remoteService,
		agentFactory,
		builder.MetricsReporters,
		builder.HandlerHooks,
		handlerPool,
	)

	app := NewApp(
		builder.ServerMode,
		builder.Serializer,
		builder.acceptors,
		builder.DieChan,
		builder.Router,
		builder.Server,
		builder.RPCClient,
		builder.RPCServer,
		builder.Worker,
		builder.ServiceDiscovery,
		remoteService,
		handlerService,
		builder.Groups,
		builder.SessionPool,
		builder.MetricsReporters,
		builder.Config.Pitaya,
	)
	app.conf = builder.conf
	return app
}

// NewDefaultApp returns a default pitaya app instance
func NewDefaultApp(isFrontend bool, serverType string, serverMode ServerMode, serverMetadata map[string]string, config config.BuilderConfig) Pitaya {
	builder := NewDefaultBuilder(isFrontend, serverType, serverMode, serverMetadata, config)
	return builder.Build()
}

func configureDefaultPipelines(handlerHooks *pipeline.HandlerHooks) {
	handlerHooks.BeforeHandler.PushBack(defaultpipelines.StructValidatorInstance.Validate)
}

func addDefaultPrometheus(config config.PrometheusConfig, customMetrics models.CustomMetricsSpec, reporters []metrics.Reporter, serverType string) []metrics.Reporter {
	prometheus, err := CreatePrometheusReporter(serverType, config, customMetrics)
	if err != nil {
		logger.Zap.Error("failed to start prometheus metrics reporter, skipping", zap.Error(err))
	} else {
		reporters = append(reporters, prometheus)
	}
	return reporters
}

func addDefaultStatsd(config config.StatsdConfig, reporters []metrics.Reporter, serverType string) []metrics.Reporter {
	statsd, err := CreateStatsdReporter(serverType, config)
	if err != nil {
		logger.Zap.Error("failed to start statsd metrics reporter, skipping", zap.Error(err))
	} else {
		reporters = append(reporters, statsd)
	}
	return reporters
}
