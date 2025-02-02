package server

import (
	"context"
	"fmt"
	"github.com/gorilla/mux"
	"github.com/hyperjumptech/hansip/internal/config"
	"github.com/hyperjumptech/hansip/internal/connector"
	"github.com/hyperjumptech/hansip/internal/endpoint"
	"github.com/hyperjumptech/hansip/internal/gzip"
	"github.com/hyperjumptech/hansip/internal/mailer"
	"github.com/hyperjumptech/hansip/pkg/helper"
	"github.com/hyperjumptech/jiffy"
	"github.com/rs/cors"
	log "github.com/sirupsen/logrus"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
)

var (
	// Router instance of gorilla mux.Router
	Router *mux.Router

	// TokenFactory will handle token creation and validation
	TokenFactory helper.TokenFactory
)

// GetJwtTokenFactory return an instance of JWT TokenFactory.
func GetJwtTokenFactory() helper.TokenFactory {
	accessDuration, err := jiffy.DurationOf(config.Get("token.access.duration"))
	if err != nil {
		panic(err)
	}
	refreshDuration, err := jiffy.DurationOf(config.Get("token.refresh.duration"))
	if err != nil {
		panic(err)
	}

	tokenFactory := helper.NewTokenFactory(
		config.Get("token.crypt.key"),
		config.Get("token.crypt.method"),
		config.Get("token.issuer"),
		accessDuration,
		refreshDuration)

	return tokenFactory
}

// InitializeRouter initializes Gorilla Mux and all handler, including Database and Mailer connector
func InitializeRouter() {
	log.Info("Initializing server")
	Router = mux.NewRouter()

	if config.GetBoolean("server.http.cors.enable") {
		log.Info("CORS handling is enabled")
		options := cors.Options{
			AllowedOrigins:     strings.Split(config.Get("server.http.cors.allow.origins"), ","),
			AllowedHeaders:     strings.Split(config.Get("server.http.cors.allow.headers"), ","),
			AllowCredentials:   config.GetBoolean("server.http.cors.allow.credential"),
			AllowedMethods:     strings.Split(config.Get("server.http.cors.allow.method"), ","),
			ExposedHeaders:     strings.Split(config.Get("server.http.cors.exposed.headers"), ","),
			OptionsPassthrough: config.GetBoolean("server.http.cors.optionpassthrough"),
			MaxAge:             config.GetInt("server.http.cors.maxage"),
		}
		log.Infof("    AllowedOrigins     : %s", strings.Join(options.AllowedOrigins, ","))
		log.Infof("    AllowedHeaders     : %s", strings.Join(options.AllowedHeaders, ","))
		log.Infof("    AllowedMethods     : %s", strings.Join(options.AllowedMethods, ","))
		log.Infof("    ExposedHeaders     : %s", strings.Join(options.ExposedHeaders, ","))
		log.Infof("    AllowCredentials   : %v", options.AllowCredentials)
		log.Infof("    OptionsPassthrough : %v", options.OptionsPassthrough)
		log.Infof("    MaxAge : %d", options.MaxAge)
		c := cors.New(options)
		Router.Use(c.Handler)
		Router.Use(endpoint.CorsMiddleware)
		gzipFilter := gzip.NewGzipEncoderFilter(true, 300)
		Router.Use(gzipFilter.DoFilter)
	}

	Router.Use(endpoint.ClientIPResolverMiddleware, endpoint.TransactionIDMiddleware, endpoint.JwtMiddleware)

	if config.Get("db.type") == "MYSQL" {
		log.Warnf("Using MYSQL")
		endpoint.UserRepo = connector.GetMySQLDBInstance()
		endpoint.GroupRepo = connector.GetMySQLDBInstance()
		endpoint.RoleRepo = connector.GetMySQLDBInstance()
		endpoint.UserGroupRepo = connector.GetMySQLDBInstance()
		endpoint.UserRoleRepo = connector.GetMySQLDBInstance()
		endpoint.GroupRoleRepo = connector.GetMySQLDBInstance()
		endpoint.TenantRepo = connector.GetMySQLDBInstance()
		endpoint.RevocationRepo = connector.GetMySQLDBInstance()
	} else if config.Get("db.type") == "SQLITE" {
		log.Warnf("Using SQLITE")
		endpoint.UserRepo = connector.GetSqliteDBInstance()
		endpoint.GroupRepo = connector.GetSqliteDBInstance()
		endpoint.RoleRepo = connector.GetSqliteDBInstance()
		endpoint.UserGroupRepo = connector.GetSqliteDBInstance()
		endpoint.UserRoleRepo = connector.GetSqliteDBInstance()
		endpoint.GroupRoleRepo = connector.GetSqliteDBInstance()
		endpoint.TenantRepo = connector.GetSqliteDBInstance()
		endpoint.RevocationRepo = connector.GetSqliteDBInstance()
	} else {
		panic(fmt.Sprintf("unknown database type %s. Correct your configuration 'db.type' or env-var 'AAA_DB_TYPE'. allowed values are INMEMORY or MYSQL", config.Get("db.type")))
	}

	if config.Get("mailer.type") == "DUMMY" {
		endpoint.EmailSender = &connector.DummyMailSender{}
	} else if config.Get("mailer.type") == "SENDMAIL" {
		endpoint.EmailSender = &connector.SendMailSender{
			Host:     config.Get("mailer.sendmail.host"),
			Port:     config.GetInt("mailer.sendmail.port"),
			User:     config.Get("mailer.sendmail.user"),
			Password: config.Get("mailer.sendmail.password"),
		}
	} else if config.Get("mailer.type") == "SENDGRID" {
		endpoint.EmailSender = &connector.SendGridSender{
			Token: config.Get("mailer.sendgrid.token"),
		}
	} else {
		panic(fmt.Sprintf("unknown mailer type %s. Correct your configuration 'mailer.type' or env-var 'AAA_MAILER_TYPE'. allowed values are DUMMY, SENDMAIL or SENDGRID", config.Get("mailer.type")))
	}
	mailer.Sender = endpoint.EmailSender

	TokenFactory = GetJwtTokenFactory()
	endpoint.TokenFactory = TokenFactory
	endpoint.TokenFactory = TokenFactory
	endpoint.InitializeRouter(Router)
	Walk()
}

func configureLogging() {
	lLevel := config.Get("server.log.level")
	fmt.Println("Setting log level to ", lLevel)
	switch strings.ToUpper(lLevel) {
	default:
		fmt.Println("Unknown level [", lLevel, "]. Log level set to ERROR")
		log.SetLevel(log.ErrorLevel)
	case "TRACE":
		log.SetLevel(log.TraceLevel)
	case "DEBUG":
		log.SetLevel(log.DebugLevel)
	case "INFO":
		log.SetLevel(log.InfoLevel)
	case "WARN":
		log.SetLevel(log.WarnLevel)
	case "ERROR":
		log.SetLevel(log.ErrorLevel)
	case "FATAL":
		log.SetLevel(log.FatalLevel)
	}
}

// Start this server
func Start() {
	configureLogging()
	log.Infof("Starting Hansip")
	startTime := time.Now()

	InitializeRouter()
	go mailer.Start()

	var wait time.Duration

	graceShut, err := jiffy.DurationOf(config.Get("server.timeout.graceshut"))
	if err != nil {
		panic(err)
	}
	wait = graceShut
	WriteTimeout, err := jiffy.DurationOf(config.Get("server.timeout.write"))
	if err != nil {
		panic(err)
	}
	ReadTimeout, err := jiffy.DurationOf(config.Get("server.timeout.read"))
	if err != nil {
		panic(err)
	}
	IdleTimeout, err := jiffy.DurationOf(config.Get("server.timeout.idle"))
	if err != nil {
		panic(err)
	}

	address := fmt.Sprintf("%s:%s", config.Get("server.host"), config.Get("server.port"))
	log.Info("Server binding to ", address)

	srv := &http.Server{
		Addr: address,
		// Good practice to set timeouts to avoid Slowloris attacks.
		WriteTimeout: WriteTimeout,
		ReadTimeout:  ReadTimeout,
		IdleTimeout:  IdleTimeout,
		Handler:      Router, // Pass our instance of gorilla/mux in.
	}
	// Run our server in a goroutine so that it doesn't block.
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			log.Println(err)
		}
	}()

	c := make(chan os.Signal, 1)
	// We'll accept graceful shutdowns when quit via SIGINT (Ctrl+C)
	// SIGKILL, SIGQUIT or SIGTERM (Ctrl+/) will not be caught.
	signal.Notify(c, os.Interrupt)

	// Block until we receive our signal.
	<-c

	mailer.Stop()

	// Create a deadline to wait for.
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	// Doesn't block if no connections, but will otherwise wait
	// until the timeout deadline.
	srv.Shutdown(ctx)
	// Optionally, you could run srv.Shutdown in a goroutine and block on
	// <-ctx.Done() if your application should wait for other services
	// to finalize based on context cancellation.
	dur := time.Now().Sub(startTime)
	durDesc := jiffy.DescribeDuration(dur, jiffy.NewWant())
	log.Infof("Shutting down. This Hansip been protecting the world for %s", durDesc)
	os.Exit(0)
}

// Walk and show all endpoint that available on this server
func Walk() {
	err := Router.Walk(func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
		pathTemplate, err := route.GetPathTemplate()
		if err != nil {
			log.Error(err)
		}
		methods, err := route.GetMethods()
		if err != nil {
			log.Error(err)
		}

		log.Infof("Route : %s [%s]", pathTemplate, strings.Join(methods, ","))
		return nil
	})

	if err != nil {
		log.Error(err)
	}
}
