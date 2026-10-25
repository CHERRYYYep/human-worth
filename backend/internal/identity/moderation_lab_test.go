//go:build lab

package identity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type kindReview struct {
	ID, Status, DeliveryState string
	Reason                    *string
	Decision                  *string
	EntryIDs                  []string `json:"entryIds"`
	Submission                struct {
		ID, State string
		Revision  int64
	}
}

// Agent Self-Claimed: synthetic accounts/session setup only. ALL review commands
// use HTTPS Gateway -> Identity -> REAL Moderation/Content Pods and their workers.
func TestKindModerationDeployment(t *testing.T) {
	l := newKindLab(t)
	forwarded := l.client
	address := os.Getenv("HUMAN_WORTH_VERIFY_GATEWAY_ADDRESS")
	if address == "" {
		t.Fatal("real validation Service/NodePort address required")
	}
	serviceClient := l.httpClientAddress(address)
	serviceClient.Transport.(*http.Transport).DisableKeepAlives = true
	l.client = serviceClient
	user, other, admin := l.user(), l.user(), l.user()
	l.sql(fmt.Sprintf(`UPDATE identity.accounts SET role='admin' WHERE id='%s';`, admin.account))
	var pods struct {
		Items []struct {
			Metadata struct{ Name string }
			Status   struct{ PodIP, NodeName string }
		}
	}
	kindDecode(t, l.kube("", "get", "pods", "-l", "app=moderation", "-o", "json"), &pods)
	if len(pods.Items) != 2 {
		t.Fatal("real Moderation two replicas required")
	}
	data := []byte("private material inspected by the real moderator service\n")
	var upload kindUpload
	body := fmt.Sprintf(`{"filename":"review.txt","mediaType":"text/plain","expectedBytes":%d,"sha256":"%x"}`, len(data), sha256.Sum256(data))
	kindDecode(t, l.request(l.client, user, "POST", "/api/me/assets/upload-sessions", body, map[string]string{"Idempotency-Key": "kind-review-upload-01"}, 201), &upload)
	l.request(l.client, user, "PUT", "/api/me/assets/upload-sessions/"+upload.UploadSession.ID+"/content", string(data), map[string]string{"Content-Type": "application/octet-stream", "Content-Range": fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data))}, 200)
	l.request(l.client, user, "POST", "/api/me/assets/"+upload.Asset.ID+"/complete", `{"uploadSessionId":"`+upload.UploadSession.ID+`"}`, nil, 200)
	draft := `{"title":"real review task","summary":"","description":"body","entries":[{"side":"human","title":"human","source":"self","artifacts":[{"kind":"file","assetId":"` + upload.Asset.ID + `"}],"permissions":{"cloudUse":false}},{"side":"agent","title":"agent","source":"model","artifacts":[{"kind":"file","assetId":"` + upload.Asset.ID + `"}],"permissions":{"cloudUse":false},"agentConfiguration":{"model":"test"}}]}`
	var task struct {
		ID, State string
		Revision  int64
		ReviewID  *string
	}
	kindDecode(t, l.request(l.client, user, "POST", "/api/tasks", draft, map[string]string{"Idempotency-Key": "kind-review-draft-01"}, 201), &task)
	reviewPath := "/api/me/tasks/" + task.ID + "/review-requests"
	publicDetail := "/api/tasks/" + task.ID
	publicEntries := publicDetail + "/entries"
	hiddenPublic := func() {
		t.Helper()
		for _, p := range []string{publicDetail, publicEntries} {
			l.request(serviceClient, kindUser{}, "GET", p, "", nil, 404)
		}
	}
	hiddenPublic()

	l.request(l.client, other, "POST", reviewPath, `{"expectedRevision":1}`, nil, 404)
	l.request(l.client, user, "GET", "/api/admin/reviews", "", nil, 403)
	l.request(l.client, kindUser{}, "GET", "/api/admin/reviews", "", nil, 401)
	var incomplete struct{ ID string }
	kindDecode(t, l.request(l.client, user, "POST", "/api/tasks", `{"title":"incomplete","summary":"","description":"","entries":[]}`, map[string]string{"Idempotency-Key": "kind-review-missing1"}, 201), &incomplete)
	l.request(l.client, user, "POST", "/api/me/tasks/"+incomplete.ID+"/review-requests", `{"expectedRevision":1}`, nil, 422)
	restore := func(name string) {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		defer cancel()
		if err := l.command(ctx, "", "scale", "deployment/"+name, "--replicas=2").Run(); err != nil {
			t.Error(err)
			return
		}
		if err := l.command(ctx, "", "rollout", "status", "deployment/"+name, "--timeout=120s").Run(); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { restore("content"); restore("moderation") })
	// Positive control: synthetic test client configuration supports only the
	// real Identity flow/redirect, not Google exchange or a successful login.
	loginClient := *serviceClient
	loginClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	l.request(&loginClient, kindUser{}, "GET", "/api/auth/google", "", nil, 302)
	// Stop real Moderation Pods, not fake gRPC. Sending review stays durable.
	l.kube("", "scale", "deployment/moderation", "--replicas=0")
	l.kube("", "wait", "--for=delete", "pod", "-l", "app=moderation", "--timeout=90s")
	// Outlast readiness failureThreshold/period. Fresh HTTPS connections use
	// NodePort -> Service -> ready EndpointSlices, never port-forward to a Pod.
	time.Sleep(15 * time.Second)
	var endpoints struct {
		Items []struct {
			Endpoints []struct{ Conditions struct{ Ready *bool } }
		}
	}
	kindDecode(t, l.kube("", "get", "endpointslices", "-l", "kubernetes.io/service-name=gateway", "-o", "json"), &endpoints)
	ready := 0
	for _, slice := range endpoints.Items {
		for _, endpoint := range slice.Endpoints {
			if endpoint.Conditions.Ready != nil && *endpoint.Conditions.Ready {
				ready++
			}
		}
	}
	if ready != 2 {
		t.Fatalf("Moderation outage removed Gateway ready endpoints: %d", ready)
	}
	l.request(serviceClient, user, "GET", "/api/me", "", nil, 200)
	l.request(serviceClient, user, "GET", "/api/me/tasks/"+task.ID, "", nil, 200)
	l.request(serviceClient, admin, "GET", "/api/admin/reviews", "", nil, 503, 504)
	l.request(serviceClient, user, "POST", "/api/tasks", `{"title":"outage draft","summary":"","description":"","entries":[]}`, map[string]string{"Idempotency-Key": "kind-review-outage-draft"}, 201)
	var outageUpload kindUpload
	kindDecode(t, l.request(serviceClient, user, "POST", "/api/me/assets/upload-sessions", body, map[string]string{"Idempotency-Key": "kind-review-outage-upload"}, 201), &outageUpload)
	l.request(serviceClient, user, "PUT", "/api/me/assets/upload-sessions/"+outageUpload.UploadSession.ID+"/content", string(data), map[string]string{"Content-Type": "application/octet-stream", "Content-Range": fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data))}, 200)
	l.request(serviceClient, user, "POST", "/api/me/assets/"+outageUpload.Asset.ID+"/complete", `{"uploadSessionId":"`+outageUpload.UploadSession.ID+`"}`, nil, 200)
	l.request(&loginClient, kindUser{}, "GET", "/api/auth/google", "", nil, 302)
	kindDecode(t, l.request(l.client, user, "POST", reviewPath, `{"expectedRevision":1}`, nil, 200), &task)
	if task.ReviewID == nil || task.State != "pending_review" {
		t.Fatal("not durably pending")
	}
	firstID := *task.ReviewID
	hiddenPublic()
	if l.sql(`SELECT count(*) FROM content.review_outbox WHERE id='`+firstID+`' AND delivered_at IS NULL`) != "1" {
		t.Fatal("missing durable outbox during outage")
	}
	l.request(l.client, user, "PUT", "/api/me/tasks/"+task.ID, `{"expectedRevision":1,"content":`+draft+`}`, nil, 409)
	restore("moderation")
	l.waitModerationRecovery(admin)
	t.Log("PASS: Moderation outage preserves both ready Gateway endpoints and login/session/draft/upload/submission via real NodePort/Service; review list alone reports dependency unavailability or deadline")
	// Tolerate bounded discovery/lease recovery, never loosen individual RPC deadline.
	poll := func(id, want string) kindReview {
		t.Helper()
		deadline := time.Now().Add(105 * time.Second)
		for {
			req, err := http.NewRequestWithContext(t.Context(), "GET", "https://worth.oopsbox.cn/api/admin/reviews/"+id, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(&http.Cookie{Name: "__Host-human-worth-session", Value: admin.session})
			response, err := l.client.Do(req)
			if err == nil {
				raw, e := io.ReadAll(response.Body)
				response.Body.Close()
				var r kindReview
				if e == nil && response.StatusCode == 200 && json.Unmarshal(raw, &r) == nil && r.Status == want {
					return r
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("moderation state did not recover: " + want)
			}
			time.Sleep(300 * time.Millisecond)
		}
	}
	pending := poll(firstID, "pending")
	if pending.DeliveryState != "undecided" {
		t.Fatal("new case already decided")
	}
	l.request(l.client, admin, "GET", "/api/admin/reviews?limit=1", "", nil, 200)
	assetPath := "/api/admin/reviews/" + firstID + "/assets/" + upload.Asset.ID + "/content"
	if actual := l.request(l.client, admin, "GET", assetPath, "", nil, 200); !bytes.Equal(actual, data) {
		t.Fatal("pending review file mismatch")
	}
	l.request(l.client, user, "GET", assetPath, "", map[string]string{"X-Role": "admin", "X-Owner-ID": user.account}, 403)
	// Stop Content after case creation. Real Moderation persists decision/outbox,
	// reports 202/pending, then resumes automatically when Content returns.
	l.kube("", "scale", "deployment/content", "--replicas=0")
	// Existing Content-outage receipt test deliberately uses the diagnostic
	// Pod tunnel; only the independent Moderation-outage test claims Service HA.
	l.client = forwarded
	l.kube("", "wait", "--for=delete", "pod", "-l", "app=content", "--timeout=90s")
	var rejected kindReview
	kindDecode(t, l.request(l.client, admin, "POST", "/api/admin/reviews/"+firstID+"/decision", `{"decision":"reject","reason":"clarify provenance"}`, nil, 202), &rejected)
	if rejected.Status != "pending" || rejected.DeliveryState != "pending" {
		t.Fatal("decision declared effective before Content receipt")
	}
	l.request(l.client, admin, "GET", assetPath, "", nil, 503, 504)
	restore("content")
	assetClients := l.assetRecoveryClients("review-recovery")
	l.waitFileAuthorization(assetClients, admin)
	// Separate clients resolve Content independently. Rollout/Moderation receipt
	// does not imply Asset's or Gateway's gRPC resolver has recovered yet.
	recoveredRead := func(actor kindUser, path string, expected int) {
		t.Helper()
		deadline := time.Now().Add(105 * time.Second)
		for {
			req, err := http.NewRequestWithContext(t.Context(), "GET", "https://worth.oopsbox.cn"+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(&http.Cookie{Name: "__Host-human-worth-session", Value: actor.session})
			response, err := l.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if response.StatusCode == expected {
				return
			}
			if (response.StatusCode != 503 && response.StatusCode != 504) || time.Now().After(deadline) {
				t.Fatalf("recovered GET %s: got %d, want %d", path, response.StatusCode, expected)
			}
			l.dependencyFailure(response.StatusCode, body)
			time.Sleep(300 * time.Millisecond)
		}
	}
	rejected = poll(firstID, "rejected")
	if rejected.DeliveryState != "applied" {
		t.Fatal("no Content receipt")
	}
	l.request(l.client, admin, "POST", "/api/admin/reviews/"+firstID+"/decision", `{"decision":"reject","reason":"clarify provenance"}`, nil, 200)
	recoveredRead(admin, assetPath, 404)
	recoveredRead(user, "/api/me/tasks/"+task.ID, 200)
	l.kube("", "rollout", "status", "deployment/gateway", "--timeout=120s")
	l.client = serviceClient
	hiddenPublic()
	// Rejected editing creates a new version; old immutable case survives.
	kindDecode(t, l.request(l.client, user, "PUT", "/api/me/tasks/"+task.ID, `{"expectedRevision":1,"content":`+draft+`}`, nil, 200), &task)
	kindDecode(t, l.request(l.client, user, "POST", reviewPath, `{"expectedRevision":2}`, nil, 200), &task)
	secondID := *task.ReviewID
	if firstID == secondID {
		t.Fatal("resubmit reused old case")
	}
	poll(secondID, "pending")
	for _, client := range assetClients {
		actual := l.request(client, admin, "GET", "/api/admin/reviews/"+secondID+"/assets/"+upload.Asset.ID+"/content", "", nil, 200)
		if !bytes.Equal(actual, data) {
			t.Fatal("pending file differs through recovered Asset caller")
		}
	}
	var old struct {
		ReviewID *string
		State    string
		Revision int64
	}
	kindDecode(t, l.request(l.client, user, "POST", reviewPath, `{"expectedRevision":1}`, nil, 200), &old)
	if old.ReviewID == nil || *old.ReviewID != firstID || old.Revision != 1 || old.State != "rejected" {
		t.Fatal("old review retry overwrote new version")
	}
	// Retry approvals concurrently against the deployed two-instance service.
	rawDecision := func(body string) int {
		req, err := http.NewRequestWithContext(t.Context(), "POST", "https://worth.oopsbox.cn/api/admin/reviews/"+secondID+"/decision", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://worth.oopsbox.cn")
		req.Header.Set("X-CSRF-Token", admin.csrf)
		req.AddCookie(&http.Cookie{Name: "__Host-human-worth-session", Value: admin.session})
		r, err := l.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		io.Copy(io.Discard, r.Body)
		return r.StatusCode
	}
	// The final approved file needs a deterministic winner, so race identical
	// approvals here; opposite first decisions are covered by PostgreSQL integration.
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for range 2 {
		wg.Go(func() { results <- rawDecision(`{"decision":"approve","reason":"材料完整，审核通过"}`) })
	}
	wg.Wait()
	for range 2 {
		code := <-results
		if code != 202 && code != 200 {
			t.Fatalf("approval retry %d", code)
		}
	}
	l.request(l.client, admin, "POST", "/api/admin/reviews/"+secondID+"/decision", `{"decision":"reject","reason":"late conflicting decision"}`, nil, 409)
	approved := poll(secondID, "approved")
	if approved.DeliveryState != "applied" || len(approved.EntryIDs) != 2 {
		t.Fatal("approval lacks stable snapshot/receipt")
	}
	if approved.Reason == nil || *approved.Reason != "材料完整，审核通过" {
		t.Fatal("approval reason lost")
	}
	l.request(l.client, admin, "POST", "/api/admin/reviews/"+secondID+"/decision", `{"decision":"approve","reason":"材料完整，审核通过"}`, nil, 200)
	l.request(l.client, admin, "POST", "/api/admin/reviews/"+secondID+"/decision", `{"decision":"approve","reason":"different reason"}`, nil, 409)
	publicPath := "/api/tasks/" + task.ID + "/entries/" + approved.EntryIDs[0] + "/assets/" + upload.Asset.ID + "/content"
	if actual := l.request(l.client, kindUser{}, "GET", publicPath, "", nil, 200); !bytes.Equal(actual, data) {
		t.Fatal("approved file mismatch")
	}
	l.request(l.client, admin, "GET", "/api/admin/reviews/"+secondID+"/assets/"+upload.Asset.ID+"/content", "", nil, 404)
	for _, client := range assetClients {
		l.request(client, admin, "GET", "/api/admin/reviews/"+secondID+"/assets/"+upload.Asset.ID+"/content", "", nil, 404)
	}
	// Release these isolated probes before the later two Content probes; keep
	// the existing disposable namespace CPU/memory quota unchanged.
	for i := range assetClients {
		l.kube("", "delete", "pod", fmt.Sprintf("review-recovery-%d", i), "--wait=true")
	}
	if l.sql(`SELECT count(*) FROM content.review_requests WHERE task_id='`+task.ID+`'`) != "2" || l.sql(`SELECT count(*) FROM moderation.reviews WHERE task_id='`+task.ID+`'`) != "2" {
		t.Fatal("history duplicated/lost")
	}
	// Force replay of an already delivered message as an at-least-once transport
	// fault; real workers retry the SAME durable ID, never create a new decision.
	l.sql(`UPDATE moderation.decision_outbox SET delivered_at=NULL,available_at=clock_timestamp()-interval '1 second' WHERE id='` + secondID + `';`)
	deadline := time.Now().Add(30 * time.Second)
	for l.sql(`SELECT count(*) FROM moderation.decision_outbox WHERE id='`+secondID+`' AND delivered_at IS NOT NULL`) != "1" {
		if time.Now().After(deadline) {
			t.Fatal("durable decision replay not acknowledged")
		}
		time.Sleep(250 * time.Millisecond)
	}
	if l.sql(`SELECT count(*) FROM content.review_decision_receipts WHERE review_id='`+secondID+`'`) != "1" {
		t.Fatal("replay duplicated execution")
	}

	// Stage 5: real formal approval above, then public read through both Content
	// replicas and normal Service while Moderation is truly offline.
	type browsePage struct {
		Items []struct {
			ID         string
			ApprovedAt *string
		}
		NextCursor *string
	}
	checkPublic := func(client *http.Client) string {
		t.Helper()
		raw := l.request(client, kindUser{}, "GET", publicDetail, "", nil, 200)
		var detail map[string]any
		kindDecode(t, raw, &detail)
		if detail["title"] != "real review task" || detail["revision"] != float64(2) || detail["heat"] != nil || detail["publishedAt"] == nil || len(detail["taskAttachments"].([]any)) != 0 {
			t.Fatal("invalid public detail", detail)
		}
		for _, denied := range []string{"agentConfiguration", "permissions", "reviewer", "deliveryState", "材料完整"} {
			if bytes.Contains(raw, []byte(denied)) {
				t.Fatal("public data leak", denied)
			}
		}
		var first, second browsePage
		kindDecode(t, l.request(client, kindUser{}, "GET", publicEntries+"?limit=1", "", nil, 200), &first)
		if len(first.Items) != 1 || first.Items[0].ID != approved.EntryIDs[0] || first.Items[0].ApprovedAt == nil || first.NextCursor == nil {
			t.Fatal("first public page invalid")
		}
		kindDecode(t, l.request(client, kindUser{}, "GET", publicEntries+"?limit=1&cursor="+*first.NextCursor, "", nil, 200), &second)
		if len(second.Items) != 1 || second.Items[0].ID != approved.EntryIDs[1] || second.NextCursor != nil {
			t.Fatal("second public page invalid")
		}
		return *first.NextCursor
	}
	cursor := checkPublic(serviceClient)
	// Pin only the Content RPC target in isolated probe gateways; never use
	// these diagnostic tunnels to claim Service outage/readiness correctness.
	var contentPods struct {
		Items []struct{ Status struct{ PodIP string } }
	}
	kindDecode(t, l.kube("", "get", "pods", "-l", "app=content", "-o", "json"), &contentPods)
	if len(contentPods.Items) != 2 {
		t.Fatal("two Content Pods required")
	}
	var contentClients []*http.Client
	for i, pod := range contentPods.Items {
		client := l.pinnedGatewayTarget(fmt.Sprintf("public-content-probe-%d", i), "CONTENT_TARGET", pod.Status.PodIP)
		contentClients = append(contentClients, client)
		checkPublic(client)
	}
	l.kube("", "scale", "deployment/moderation", "--replicas=0")
	l.kube("", "wait", "--for=delete", "pod", "-l", "app=moderation", "--timeout=90s")
	time.Sleep(15 * time.Second)
	checkPublic(serviceClient)
	if actual := l.request(serviceClient, kindUser{}, "GET", publicPath, "", nil, 200); !bytes.Equal(actual, data) {
		t.Fatal("approved S3 read depends on Moderation")
	}
	l.request(serviceClient, admin, "GET", "/api/admin/reviews", "", nil, 503, 504)
	// Controlled state tightening fixture only. No public write/admin bypass
	// API is introduced; real approval remains the Moderation workflow above.
	l.sql(`UPDATE content.reviewed_entries SET state='approval_revoked' WHERE id='` + approved.EntryIDs[1] + `'; UPDATE content.reviewed_tasks SET revision=revision+1 WHERE task_id='` + task.ID + `';`)
	revokedFile := publicDetail + "/entries/" + approved.EntryIDs[1] + "/assets/" + upload.Asset.ID + "/content"
	for _, client := range append(contentClients, serviceClient) {
		var next browsePage
		kindDecode(t, l.request(client, kindUser{}, "GET", publicEntries+"?limit=1&cursor="+cursor, "", nil, 200), &next)
		if len(next.Items) != 0 || next.NextCursor != nil {
			t.Fatal("revocation not rechecked on next page")
		}
		l.request(client, kindUser{}, "GET", revokedFile, "", nil, 404)
	}
	l.sql(`UPDATE content.reviewed_tasks SET state='taken_down',revision=revision+1 WHERE task_id='` + task.ID + `';`)
	for _, client := range append(contentClients, serviceClient) {
		for _, path := range []string{publicDetail, publicEntries, publicEntries + "?cursor=" + cursor, publicPath} {
			l.request(client, kindUser{}, "GET", path, "", nil, 404)
		}
	}
	restore("moderation")
	l.waitModerationRecovery(admin)
	// At-least-once replay via REAL Moderation workers must not revive either
	// current task or entry visibility, even though the stored receipt is approve.
	l.sql(`UPDATE moderation.decision_outbox SET delivered_at=NULL,available_at=clock_timestamp()-interval '1 second' WHERE id='` + secondID + `';`)
	deadline = time.Now().Add(105 * time.Second)
	for l.sql(`SELECT count(*) FROM moderation.decision_outbox WHERE id='`+secondID+`' AND delivered_at IS NOT NULL`) != "1" {
		if time.Now().After(deadline) {
			t.Fatal("replay recovery timeout")
		}
		time.Sleep(300 * time.Millisecond)
	}
	hiddenPublic()
	l.request(serviceClient, kindUser{}, "GET", publicPath, "", nil, 404)
	t.Log("PASS: stage 5 frozen public detail/keyset pages through both Content Pods; real Service/NodePort reads during Moderation outage; revoke/takedown denies new pages/files; real worker replay cannot revive")
	// No synthetic reviewer certificate is used for this formal workflow.
	t.Log("PASS: real Moderation Pods, HTTP/mTLS/PostgreSQL/S3, outbox outage/recovery, admin-only pending files, rejection/resubmit/history, receipt replay and approved file")
}
