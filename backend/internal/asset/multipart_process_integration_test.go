//go:build integration

package asset

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/platform"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type multipartProcessConfig struct {
	DSN, Cert, Key, CA, AddressFile, PartWrittenFile string
	S3                                               S3Config
}

// A separate OS process has an actual in-flight DB transaction when killed,
// after S3 confirms part 2 but before the SDK can return it to the application.
func TestMultipartProcessRestart(t *testing.T) {
	if path := os.Getenv("HW_MULTIPART_PROCESS_CONFIG"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("read helper configuration")
		}
		var config multipartProcessConfig
		if err = json.Unmarshal(raw, &config); err != nil {
			t.Fatal("decode helper configuration")
		}
		db, err := pgxpool.New(t.Context(), config.DSN)
		if err != nil {
			t.Fatal("helper database")
		}
		defer db.Close()
		store, err := NewS3Storage(config.S3)
		if err != nil {
			t.Fatal(err)
		}
		if config.PartWrittenFile != "" {
			options := store.client.Options()
			original := options.HTTPClient
			options.HTTPClient = &http.Client{Transport: s3RoundTrip(func(r *http.Request) (*http.Response, error) {
				response, err := original.Do(r)
				if err == nil && response.StatusCode == 200 && r.Method == "PUT" && r.URL.Query().Get("partNumber") == "2" {
					io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if err = os.WriteFile(config.PartWrittenFile, []byte("part written before DB confirmation"), 0600); err != nil {
						return nil, err
					}
					select {} // The parent sends SIGKILL; no graceful cleanup runs.
				}
				return response, err
			})}
			store.client = s3.New(options)
		}
		tlsConfig, err := platform.TLS(config.Cert, config.Key, config.CA, "")
		if err != nil {
			t.Fatal(err)
		}
		server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)), grpc.UnaryInterceptor(Authorization), grpc.StreamInterceptor(AuthorizationStream), grpc.MaxRecvMsgSize(platform.AssetUploadMessageBytes))
		pb.RegisterAssetServiceServer(server, &Server{DB: db, Storage: store, Identity: &fakeIdentityClient{revoked: map[string]bool{}}, BrowserUpload: browserUploadUseCase(db, store), UploadSessions: uploadSessionUseCase(db), CompleteUseCase: completeUploadUseCase(db, store), AbortUseCase: abortUploadUseCase(db, store), ChallengeUpload: challengeUploadUseCase(db, store)})
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(config.AddressFile, []byte(listener.Addr().String()), 0600); err != nil {
			t.Fatal(err)
		}
		if err = server.Serve(listener); err != nil {
			t.Fatal(err)
		}
		return
	}
	dsn := testDatabaseURL(t)
	if dsn == "" {
		t.Skip("ASSET_TEST_DATABASE_URL_FILE is required")
	}
	store, _ := newS3Integration(t)
	f := newAssetIntegration(t, dsn)
	root := t.TempDir()
	cert, key, ca := f.pki.cert(t, "asset")
	connConfig := f.runtimeDB.Config().ConnConfig
	dsnURL := url.URL{Scheme: "postgres", Host: net.JoinHostPort(connConfig.Host, strconv.Itoa(int(connConfig.Port))), Path: "/" + connConfig.Database, User: url.UserPassword(connConfig.User, connConfig.Password), RawQuery: "sslmode=disable"}
	config := multipartProcessConfig{DSN: dsnURL.String(), Cert: cert, Key: key, CA: ca, AddressFile: filepath.Join(root, "address"), PartWrittenFile: filepath.Join(root, "part-written"), S3: S3Config{Endpoint: *store.client.Options().BaseEndpoint, Bucket: store.bucket, Region: store.client.Options().Region, ForcePathStyle: store.client.Options().UsePathStyle}}
	credential, err := store.client.Options().Credentials.Retrieve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	config.S3.AccessKey = credential.AccessKeyID
	config.S3.SecretKey = credential.SecretAccessKey
	configFile := filepath.Join(root, "config.json")
	start := func() (*exec.Cmd, pb.AssetServiceClient) {
		t.Helper()
		os.Remove(config.AddressFile)
		raw, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(configFile, raw, 0600); err != nil {
			t.Fatal(err)
		}
		command := exec.Command(os.Args[0], "-test.run=^TestMultipartProcessRestart$", "-test.timeout=45s")
		command.Env = append(os.Environ(), "HW_MULTIPART_PROCESS_CONFIG="+configFile)
		if err = command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { command.Process.Kill(); command.Wait() })
		var address []byte
		deadline := time.Now().Add(10 * time.Second)
		for {
			address, err = os.ReadFile(config.AddressFile)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("helper did not start")
			}
			time.Sleep(10 * time.Millisecond)
		}
		clientCert, clientKey, _ := f.pki.cert(t, "gateway")
		tlsConfig, err := platform.TLS(clientCert, clientKey, ca, "asset")
		if err != nil {
			t.Fatal(err)
		}
		connection, err := grpc.NewClient(string(address), grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), grpc.WithDisableRetry(), grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(platform.AssetUploadMessageBytes)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { connection.Close() })
		return command, pb.NewAssetServiceClient(connection)
	}
	command, client := start()
	data := make([]byte, maxChunkBytes+31)
	for i := range data {
		data[i] = 'x'
	}
	created := multipartCreate(t, client, data)
	if _, err = browserUpload(t, client, created.UploadSession.Id, "alice", data[:maxChunkBytes]); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := browserUploadAt(t, client, created.UploadSession.Id, "alice", maxChunkBytes, data[maxChunkBytes:])
		result <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err = os.Stat(config.PartWrittenFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("part 2 never reached S3 before DB confirmation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = command.Wait(); err == nil {
		t.Fatal("helper unexpectedly exited normally")
	}
	select {
	case err = <-result:
		if err == nil {
			t.Fatal("killed upload reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("killed RPC remained pending")
	}
	_, uploadID, offset, parts := multipartProgress(t, f, created.Asset.Id)
	if uploadID == "" || offset != maxChunkBytes || len(parts) != 1 {
		t.Fatal("SIGKILL corrupted durable progress", offset, len(parts))
	}
	config.PartWrittenFile = ""
	_, restarted := start()
	if _, err = browserUploadAt(t, restarted, created.UploadSession.Id, "alice", offset, data[maxChunkBytes:]); err != nil {
		t.Fatal(err)
	}
	completed, err := restarted.CompleteUpload(t.Context(), &pb.CompleteUploadRequest{ActorAssertion: "alice", UploadSessionId: created.UploadSession.Id})
	if err != nil || completed.GetAsset().GetSha256() != sha256Hex(data) {
		t.Fatal("OS process restart recovery failed", err)
	}
	if sha256Hex(download(t, restarted, "alice", created.Asset.Id)) != sha256Hex(data) {
		t.Fatal("restart changed bytes")
	}
}
