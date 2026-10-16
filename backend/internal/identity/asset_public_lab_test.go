//go:build lab

package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/platform"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Agent Self-Claimed: a synthetic trusted Moderation caller, NOT a Moderation
// deployment or admin-review UI. Uses ONLY the disposable namespace's test CA.
func (l *kindLab) reviewClient() contentpb.ContentServiceClient {
	l.t.Helper()
	dir := l.t.TempDir()
	key, csr, crt, ext := filepath.Join(dir, "key"), filepath.Join(dir, "csr"), filepath.Join(dir, "crt"), filepath.Join(dir, "ext")
	if err := os.WriteFile(ext, []byte("basicConstraints=CA:FALSE\nkeyUsage=digitalSignature\nextendedKeyUsage=clientAuth\nsubjectAltName=URI:spiffe://human-worth/services/moderation\n"), 0600); err != nil {
		l.t.Fatal(err)
	}
	for _, args := range [][]string{
		{"req", "-new", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes", "-keyout", key, "-out", csr, "-subj", "/CN=moderation"},
		{"x509", "-req", "-in", csr, "-CA", filepath.Join(l.state, "ca.crt"), "-CAkey", filepath.Join(l.state, "ca.key"), "-set_serial", "0x" + fmt.Sprintf("%x", time.Now().UnixNano()), "-out", crt, "-days", "1", "-extfile", ext},
	} {
		if err := exec.CommandContext(l.t.Context(), "openssl", args...).Run(); err != nil {
			l.t.Fatal("test review certificate creation failed")
		}
	}
	if err := os.Chmod(key, 0600); err != nil {
		l.t.Fatal(err)
	}
	config, err := platform.TLS(crt, key, filepath.Join(l.state, "ca.crt"), "content")
	if err != nil {
		l.t.Fatal(err)
	}
	conn, err := grpc.NewClient("127.0.0.1:"+l.forward("service/content", 8443), grpc.WithTransportCredentials(credentials.NewTLS(config)), grpc.WithDisableRetry())
	if err != nil {
		l.t.Fatal(err)
	}
	l.t.Cleanup(func() { conn.Close() })
	return contentpb.NewContentServiceClient(conn)
}

func TestKindPublicAssetDeployment(t *testing.T) {
	l := newKindLab(t)
	user := l.user()
	var pods struct {
		Items []struct{ Status struct{ PodIP string } }
	}
	kindDecode(t, l.kube("", "get", "pods", "-l", "app=asset", "-o", "json"), &pods)
	if len(pods.Items) != 2 {
		t.Fatal("expected two Asset replicas")
	}
	clients := []*http.Client{l.pinnedGateway("public-asset-a", pods.Items[0].Status.PodIP), l.pinnedGateway("public-asset-b", pods.Items[1].Status.PodIP)}
	data := []byte("approved private upload becomes a reviewed file, never a permanent URL\n")
	var upload kindUpload
	body := fmt.Sprintf(`{"filename":"approved.txt","mediaType":"text/plain","expectedBytes":%d,"sha256":"%x"}`, len(data), sha256.Sum256(data))
	kindDecode(t, l.request(clients[0], user, "POST", "/api/me/assets/upload-sessions", body, map[string]string{"Idempotency-Key": "kind-public-upload01"}, 201), &upload)
	l.request(clients[0], user, "PUT", "/api/me/assets/upload-sessions/"+upload.UploadSession.ID+"/content", string(data), map[string]string{"Content-Type": "application/octet-stream", "Content-Range": fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data))}, 200)
	l.request(clients[1], user, "POST", "/api/me/assets/"+upload.Asset.ID+"/complete", `{"uploadSessionId":"`+upload.UploadSession.ID+`"}`, nil, 200)
	body = `{"title":"reviewed bundle","summary":"","description":"","entries":[{"side":"human","title":"human","source":"self","artifacts":[{"kind":"file","assetId":"` + upload.Asset.ID + `"}],"permissions":{"cloudUse":false}},{"side":"agent","title":"agent","source":"model","artifacts":[{"kind":"link","url":"https://example.com/output"}],"permissions":{"cloudUse":false},"agentConfiguration":{"model":"test"}}]}`
	var task struct{ ID string }
	kindDecode(t, l.request(clients[0], user, "POST", "/api/tasks", body, map[string]string{"Idempotency-Key": "kind-public-draft001"}, 201), &task)
	path := func(entry string) string {
		return "/api/tasks/" + task.ID + "/entries/" + entry + "/assets/" + upload.Asset.ID + "/content"
	}
	l.request(clients[0], kindUser{}, "GET", path("ent_00000000000000000000000000000000"), "", nil, 404)
	reviewer := l.reviewClient()
	apply := func(r *contentpb.ApplyAssetReviewRequest) *contentpb.ApplyAssetReviewResponse {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		result, err := reviewer.ApplyAssetReview(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	approval := &contentpb.ApplyAssetReviewRequest{DecisionId: "kind-public-approval01", TaskId: task.ID, SubmissionRevision: 1, Decision: &contentpb.ApplyAssetReviewRequest_TaskState{TaskState: contentpb.ReviewedTaskState_REVIEWED_TASK_STATE_PUBLISHED}}
	receipt := apply(approval)
	if receipt.Revision != 1 || len(receipt.Entries) != 2 {
		t.Fatal("invalid review receipt")
	}
	entry := receipt.Entries[0].Id
	download := func(want int) {
		t.Helper()
		for _, client := range clients {
			got := l.request(client, kindUser{}, "GET", path(entry), "", nil, want)
			if want == 200 && !bytes.Equal(got, data) {
				t.Fatal("deployed public download bytes differ")
			}
		}
	}
	download(200)
	l.request(clients[0], kindUser{}, "GET", path(receipt.Entries[1].Id), "", nil, 404)
	l.request(clients[1], kindUser{}, "GET", path(entry)+"?public=true", "", nil, 400)
	revoke := &contentpb.ApplyAssetReviewRequest{DecisionId: "kind-public-revoke001", TaskId: task.ID, SubmissionRevision: 1, ExpectedRevision: 1, Decision: &contentpb.ApplyAssetReviewRequest_EntryReview{EntryReview: &contentpb.EntryAssetReview{EntryId: entry, State: contentpb.ReviewedEntryState_REVIEWED_ENTRY_STATE_APPROVAL_REVOKED}}}
	apply(revoke)
	download(404)
	if !proto.Equal(apply(approval), receipt) {
		t.Fatal("review replay changed receipt")
	}
	download(404)
	stale := proto.Clone(approval).(*contentpb.ApplyAssetReviewRequest)
	stale.DecisionId = "kind-public-stale0001"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	_, err := reviewer.ApplyAssetReview(ctx, stale)
	cancel()
	if status.Code(err) != codes.Aborted {
		t.Fatal("stale approval was accepted", err)
	}
	restore := proto.Clone(revoke).(*contentpb.ApplyAssetReviewRequest)
	restore.DecisionId = "kind-public-restore01"
	restore.ExpectedRevision = 2
	restore.GetEntryReview().State = contentpb.ReviewedEntryState_REVIEWED_ENTRY_STATE_APPROVED
	apply(restore)
	download(200)
	down := proto.Clone(approval).(*contentpb.ApplyAssetReviewRequest)
	down.DecisionId = "kind-public-down0001"
	down.ExpectedRevision = 3
	down.Decision = &contentpb.ApplyAssetReviewRequest_TaskState{TaskState: contentpb.ReviewedTaskState_REVIEWED_TASK_STATE_TAKEN_DOWN}
	apply(down)
	download(404)
	apply(approval)
	download(404)
	reopen := proto.Clone(approval).(*contentpb.ApplyAssetReviewRequest)
	reopen.DecisionId = "kind-public-reopen01"
	reopen.ExpectedRevision = 4
	apply(reopen)
	download(200)
	// Actual dependency outage. Asset remains independently ready for uploads and
	// private reads; public requests cannot fall back to cached approval.
	restoreContent := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		defer cancel()
		if err := l.command(ctx, "", "scale", "deployment/content", "--replicas=2").Run(); err != nil {
			t.Error("restore Content replicas", err)
			return
		}
		if err := l.command(ctx, "", "rollout", "status", "deployment/content", "--timeout=120s").Run(); err != nil {
			t.Error("restore Content readiness", err)
		}
	}
	t.Cleanup(restoreContent)
	l.kube("", "scale", "deployment/content", "--replicas=0")
	l.kube("", "wait", "--for=delete", "pod", "-l", "app=content", "--timeout=90s")
	download(503)
	l.request(clients[0], user, "GET", "/api/me/assets/"+upload.Asset.ID+"/content", "", nil, 200)
	if l.sql(`SELECT count(*) FROM content.reviewed_tasks WHERE task_id='`+task.ID+`' AND state='published' AND revision=5`) != "1" {
		t.Fatal("outage changed review authority")
	}
	restoreContent()
	// Headless gRPC DNS refresh is rate-limited; healthy replacement Pods do
	// not imply that every existing client has resolved their new IPs yet.
	deadline := time.Now().Add(90 * time.Second)
	for _, client := range clients {
		for {
			req, e := http.NewRequestWithContext(t.Context(), "GET", "https://worth.oopsbox.cn"+path(entry), nil)
			if e != nil {
				t.Fatal(e)
			}
			resp, e := client.Do(req)
			if e == nil {
				actual, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 && readErr == nil && bytes.Equal(actual, data) {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("public dependency did not recover")
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	t.Log("PASS: real HTTPS/mTLS/PostgreSQL/S3 public read across two Asset Pods, revoke/take-down denial and Content outage/recovery; no Moderation service or public deployment")
}
