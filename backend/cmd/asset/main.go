package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	identitypb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/identity/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/asset"
	assetapp "github.com/KDZZZZZZ/human-worth/backend/internal/asset/application"
	assetpostgres "github.com/KDZZZZZZ/human-worth/backend/internal/asset/repository/postgres"
	"github.com/KDZZZZZZ/human-worth/backend/internal/platform"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	_ "google.golang.org/grpc/balancer/roundrobin"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	if err := run(); err != nil {
		slog.Error("asset stopped", "reason", err.Error())
		os.Exit(1)
	}
}
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	dsn, err := platform.Secret("ASSET_DATABASE_URL_FILE")
	if err != nil {
		return err
	}
	db, err := platform.Database(ctx, dsn, 40*time.Second)
	if err != nil {
		return err
	}
	defer db.Close()
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		migrateCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err = asset.Migrate(migrateCtx, db); err != nil {
			return errors.New("asset migration failed; check database access and schema version")
		}
		return nil
	}
	target, err := platform.Required("IDENTITY_TARGET")
	if err != nil {
		return err
	}
	clientTLS, err := platform.TLS(os.Getenv("SERVICE_CERT_FILE"), os.Getenv("SERVICE_KEY_FILE"), os.Getenv("SERVICE_CA_FILE"), "identity")
	if err != nil {
		return err
	}
	identityConn, err := platform.GRPCClient(target, grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)), grpc.WithDisableRetry(), grpc.WithStatsHandler(otelgrpc.NewClientHandler()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(131072), grpc.MaxCallSendMsgSize(65536)))
	if err != nil {
		return errors.New("identity client initialization failed")
	}
	defer identityConn.Close()
	var contentClient contentpb.ContentServiceClient
	// Optional to preserve private-only installations; public reads fail closed
	// without this target. Do not require Content health in Asset's readiness:
	// Content itself needs Asset for private writes (no readiness dependency cycle).
	if target := os.Getenv("CONTENT_TARGET"); target != "" {
		config, e := platform.TLS(os.Getenv("SERVICE_CERT_FILE"), os.Getenv("SERVICE_KEY_FILE"), os.Getenv("SERVICE_CA_FILE"), "content")
		if e != nil {
			return e
		}
		conn, e := platform.GRPCClient(target, grpc.WithTransportCredentials(credentials.NewTLS(config)), grpc.WithDisableRetry(), grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`), grpc.WithStatsHandler(otelgrpc.NewClientHandler()), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(65536), grpc.MaxCallSendMsgSize(65536)))
		if e != nil {
			return errors.New("content client initialization failed")
		}
		defer conn.Close()
		contentClient = contentpb.NewContentServiceClient(conn)
	}
	serverTLS, err := platform.TLS(os.Getenv("SERVICE_CERT_FILE"), os.Getenv("SERVICE_KEY_FILE"), os.Getenv("SERVICE_CA_FILE"), "")
	if err != nil {
		return err
	}
	storage, storageErr := asset.StorageFromEnv()
	if storageErr != nil {
		slog.Error("object storage configuration unavailable; readiness disabled")
	}
	runtime := platform.NewRuntime("asset")
	shutdown, err := platform.Trace(ctx, "asset")
	if err != nil {
		return errors.New("telemetry initialization failed")
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shutdown(c)
	}()
	runtime.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "human_worth_database_connections", Help: "Open connections in this replica's pool"}, func() float64 { return float64(db.Stat().TotalConns()) }))
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)), grpc.ChainUnaryInterceptor(runtime.Unary, asset.Authorization), grpc.ChainStreamInterceptor(runtime.Stream, asset.AuthorizationStream), grpc.StatsHandler(otelgrpc.NewServerHandler()), grpc.MaxRecvMsgSize(platform.AssetUploadMessageBytes), grpc.MaxSendMsgSize(512<<10), grpc.MaxConcurrentStreams(64))
	browserUpload := assetapp.BrowserUploadUseCase{
		Repository:    assetpostgres.UploadRepository{DB: db},
		Storage:       storage,
		ChunkBytes:    platform.AssetUploadChunkBytes,
		MaxAssetBytes: 32 << 20,
	}
	if multipart, ok := storage.(assetapp.MultipartStorage); ok {
		browserUpload.Multipart = multipart
	}
	uploadRepository := assetpostgres.UploadRepository{DB: db}
	uploadSessions := assetapp.SessionUseCase{Repository: uploadRepository}
	completeUpload := assetapp.CompleteUploadUseCase{Repository: uploadRepository, Storage: storage}
	abortUpload := assetapp.AbortUploadUseCase{Repository: uploadRepository, Storage: storage}
	cleanupUploads := assetapp.CleanupUseCase{Repository: uploadRepository, Storage: storage}
	challengeUpload := assetapp.ChallengeUploadUseCase{Repository: uploadRepository, Storage: storage, ChunkBytes: platform.AssetUploadChunkBytes, MaxAssetBytes: 32 << 20}
	if multipart, ok := storage.(assetapp.ChallengeMultipartStorage); ok {
		challengeUpload.Multipart = multipart
	}
	pb.RegisterAssetServiceServer(server, &asset.Server{DB: db, Identity: identitypb.NewIdentityServiceClient(identityConn), Storage: storage, Content: contentClient, BrowserUpload: browserUpload, UploadSessions: uploadSessions, CompleteUseCase: completeUpload, AbortUseCase: abortUpload, ChallengeUpload: challengeUpload})
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(server, healthServer)
	ready := func(check context.Context) error {
		if err := asset.Ready(check, db); err != nil {
			return err
		}
		if storageErr != nil {
			return errors.New("object storage configuration unavailable")
		}
		if err := asset.CheckStorage(check, storage); err != nil {
			return errors.New("object storage unavailable")
		}
		response, err := healthpb.NewHealthClient(identityConn).Check(check, &healthpb.HealthCheckRequest{})
		if err != nil || response.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			return errors.New("identity unavailable")
		}
		return nil
	}
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	probe := runtime.Health(platform.Value("HEALTH_LISTEN", ":8081"), ready)
	listener, err := net.Listen("tcp", platform.Value("GRPC_LISTEN", ":8443"))
	if err != nil {
		return errors.New("gRPC listener unavailable")
	}
	failures := make(chan error, 2)
	go func() { failures <- server.Serve(listener) }()
	go func() { failures <- probe.ListenAndServe() }()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check, cancel := context.WithTimeout(ctx, 2*time.Second)
				state := healthpb.HealthCheckResponse_SERVING
				if ready(check) != nil {
					state = healthpb.HealthCheckResponse_NOT_SERVING
				}
				cancel()
				healthServer.SetServingStatus("", state)
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check, cancel := context.WithTimeout(ctx, 30*time.Second)
				if storage != nil {
					if e := cleanupUploads.Execute(check); e != nil {
						runtime.Log.Warn("object cleanup incomplete")
					}
				}
				cancel()
			}
		}
	}()
	runtime.Log.Info("started", "revision", platform.BuildVersion())
	select {
	case <-ctx.Done():
	case err = <-failures:
	}
	stop, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	probe.Shutdown(stop)
	platform.StopGRPC(server)
	if err != nil {
		return errors.New("asset listener stopped")
	}
	return nil
}
