package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"time"

	"github.com/hellofresh/janus/pkg/api"
	"github.com/hellofresh/janus/pkg/config"
	"github.com/hellofresh/janus/pkg/errors"
	httpErrors "github.com/hellofresh/janus/pkg/errors"
	"github.com/hellofresh/janus/pkg/loader"
	"github.com/hellofresh/janus/pkg/middleware"
	"github.com/hellofresh/janus/pkg/plugin"
	"github.com/hellofresh/janus/pkg/proxy"
	"github.com/hellofresh/janus/pkg/router"
	"github.com/hellofresh/janus/pkg/web"
	"github.com/hellofresh/stats-go/client"
	log "github.com/sirupsen/logrus"
)

// Server is the Janus server
type Server struct {
	server    *http.Server
	provider  api.Repository
	register  *proxy.Register
	defLoader *loader.APILoader
	started   bool

	currentConfigurations []*api.Spec
	configurationChan     chan api.ConfigurationChanged
	stopChan              chan bool
	globalConfig          *config.Specification
	statsClient           client.Client
}

// New creates a new instance of Server
func New(opts ...Option) *Server {
	s := Server{
		configurationChan: make(chan api.ConfigurationChanged, 100),
		stopChan:          make(chan bool, 1),
	}

	for _, opt := range opts {
		opt(&s)
	}

	return &s
}

// Start starts the server
func (s *Server) Start() error {
	return s.StartWithContext(context.Background())
}

// StartWithContext starts the server and Stop/Close it when context is Done
func (s *Server) StartWithContext(ctx context.Context) error {
	go func() {
		defer s.Close()
		<-ctx.Done()
		log.Info("I have to go...")
		reqAcceptGraceTimeOut := time.Duration(s.globalConfig.GraceTimeOut)
		if reqAcceptGraceTimeOut > 0 {
			log.Infof("Waiting %s for incoming requests to cease", reqAcceptGraceTimeOut)
			time.Sleep(reqAcceptGraceTimeOut)
		}
		log.Info("Stopping server gracefully")
		s.Close()
	}()
	go func() {
		if err := s.startHTTPServers(); err != nil {
			log.WithError(err).Fatal("Could not start http servers")
		}
	}()
	go func() {
		s.listenProviders(s.stopChan)
	}()
	if err := s.startProvider(ctx); err != nil {
		log.WithError(err).Fatal("Could not start providers")
	}

	return nil
}

// Wait blocks until server is shutted down.
func (s *Server) Wait() {
	<-s.stopChan
}

// Stop stops the server
func (s *Server) Stop() {
	defer log.Info("Server stopped")

	graceTimeOut := time.Duration(s.globalConfig.GraceTimeOut)
	ctx, cancel := context.WithTimeout(context.Background(), graceTimeOut)
	log.Debugf("Waiting %s seconds before killing connections...", graceTimeOut)
	if err := s.server.Shutdown(ctx); err != nil {
		log.WithError(err).Debug("Wait is over due to error")
		s.server.Close()
	}
	cancel()
	log.Debug("Server closed")

	s.stopChan <- true
}

// Close closes the server
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func(ctx context.Context) {
		<-ctx.Done()
		if ctx.Err() == context.Canceled {
			return
		} else if ctx.Err() == context.DeadlineExceeded {
			panic("Timeout while stopping janus, killing instance ✝")
		}
	}(ctx)

	close(s.stopChan)
	close(s.configurationChan)

	return s.server.Close()
}

func (s *Server) startHTTPServers() error {
	r := s.createRouter()
	// some routers may panic when have empty routes list, so add one dummy 404 route to avoid this
	if r.RoutesCount() < 1 {
		r.Any("/", httpErrors.NotFound)
	}

	s.register = proxy.NewRegister(r, proxy.Params{
		StatsClient:            s.statsClient,
		FlushInterval:          s.globalConfig.BackendFlushInterval,
		IdleConnectionsPerHost: s.globalConfig.MaxIdleConnsPerHost,
		CloseIdleConnsPeriod:   s.globalConfig.CloseIdleConnsPeriod,
	})
	s.defLoader = loader.NewAPILoader(s.register)

	return s.listenAndServe(r)
}

func (s *Server) startProvider(ctx context.Context) error {
	webServer := web.New(
		s.provider,
		web.WithPort(s.globalConfig.Web.Port),
		web.WithTLS(s.globalConfig.Web.TLS),
		web.WithCredentials(s.globalConfig.Web.Credentials),
		web.ReadOnly(s.globalConfig.Web.ReadOnly),
	)
	if err := webServer.Start(); err != nil {
		return errors.Wrap(err, "could not start Janus web API")
	}

	s.provider.Watch(ctx, s.configurationChan)
	return nil
}

func (s *Server) listenProviders(stop chan bool) {
	for {
		select {
		case <-stop:
			return
		case configMsg, ok := <-s.configurationChan:
			if !ok || configMsg.Configurations == nil {
				return
			}

			if reflect.DeepEqual(s.currentConfigurations, configMsg.Configurations) {
				log.Debug("Skipping same configuration")
				continue
			}

			s.currentConfigurations = configMsg.Configurations
			s.handleEvent(configMsg.Configurations)
		}
	}
}

func (s *Server) listenAndServe(handler http.Handler) error {
	address := fmt.Sprintf(":%v", s.globalConfig.Port)
	logger := log.WithField("address", address)
	s.server = &http.Server{Addr: address, Handler: handler}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return errors.Wrap(err, "error opening listener")
	}

	if s.globalConfig.TLS.IsHTTPS() {
		s.server.Addr = fmt.Sprintf(":%v", s.globalConfig.TLS.Port)

		if s.globalConfig.TLS.Redirect {
			go func() {
				logger.Info("Listening HTTP redirects to HTTPS")
				log.Fatal(http.Serve(listener, web.RedirectHTTPS(s.globalConfig.TLS.Port)))
			}()
		}

		logger.Info("Listening HTTPS")
		return s.server.ServeTLS(listener, s.globalConfig.TLS.CertFile, s.globalConfig.TLS.KeyFile)
	}

	logger.Info("Certificate and certificate key were not found, defaulting to HTTP")
	return s.server.Serve(listener)
}

func (s *Server) createRouter() router.Router {
	// create router with a custom not found handler
	router.DefaultOptions.NotFoundHandler = errors.NotFound
	r := router.NewChiRouterWithOptions(router.DefaultOptions)
	r.Use(
		middleware.NewStats(s.statsClient).Handler,
		middleware.NewLogger().Handler,
		middleware.NewRecovery(errors.RecoveryHandler),
		middleware.NewOpenTracing(s.globalConfig.TLS.IsHTTPS()).Handler,
	)

	if s.globalConfig.RequestID {
		r.Use(middleware.RequestID)
	}

	return r
}

func (s *Server) handleEvent(specs []*api.Spec) {
	if !s.started {
		event := plugin.OnStartup{
			StatsClient:   s.statsClient,
			Register:      s.register,
			Config:        s.globalConfig,
			Configuration: specs,
		}

		if mgoRepo, ok := s.provider.(*api.MongoRepository); ok {
			event.MongoSession = mgoRepo.Session
		}

		plugin.EmitEvent(plugin.StartupEvent, event)

		s.defLoader.RegisterAPIs(specs)
		s.started = true
		log.Info("Janus started")
	} else {
		log.Debug("Refreshing configuration")
		newRouter := s.createRouter()
		s.register.UpdateRouter(newRouter)
		s.defLoader.RegisterAPIs(specs)

		plugin.EmitEvent(plugin.ReloadEvent, plugin.OnReload{Configurations: specs})

		s.server.Handler = newRouter
		log.Debug("Configuration refresh done")
	}
}
