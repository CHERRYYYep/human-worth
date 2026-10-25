//go:build lab

package identity

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Real disposable Pods and existing Asset -> Content connections. Reproduces
// the distinction between Pod rollout and recovery of EVERY actual caller.
// No replacement authorization service, cached grant or relaxed final 404.
func TestKindFileAuthorizationRecoveryDeployment(t *testing.T) {
	l := newKindLab(t)
	admin := l.user()
	l.sql(fmt.Sprintf(`UPDATE identity.accounts SET role='admin' WHERE id='%s'`, admin.account))
	clients := l.assetRecoveryClients("file-recovery")
	l.waitFileAuthorization(clients, admin)
	assertDenied := func() {
		t.Helper()
		for _, client := range clients {
			l.request(client, kindUser{}, "GET", recoveryPublicFile, "", nil, 404)
			l.request(client, admin, "GET", recoveryReviewFile, "", nil, 404)
		}
	}
	assertDenied()
	restore := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
		defer cancel()
		if err := l.command(ctx, "", "scale", "deployment/content", "--replicas=2").Run(); err != nil {
			t.Error("restore Content", err)
			return
		}
		if err := l.command(ctx, "", "rollout", "status", "deployment/content", "--timeout=120s").Run(); err != nil {
			t.Error("restore Content rollout", err)
		}
	}
	t.Cleanup(restore)
	l.kube("", "scale", "deployment/content", "--replicas=0")
	l.kube("", "wait", "--for=delete", "pod", "-l", "app=content", "--timeout=90s")
	for _, client := range clients {
		l.request(client, kindUser{}, "GET", recoveryPublicFile, "", nil, 503, 504)
		l.request(client, admin, "GET", recoveryReviewFile, "", nil, 503, 504)
	}
	restore()
	// Check every actual Asset caller and both authorization methods; normal
	// Gateway readiness intentionally cannot depend on this circular dependency.
	l.waitFileAuthorization(clients, admin)
	assertDenied()
	// The normal NodePort still has independent Gateway -> Asset connections.
	normal := l.httpClientAddress(getenvRequired(t, "HUMAN_WORTH_VERIFY_GATEWAY_ADDRESS"))
	normal.Transport.(*http.Transport).DisableKeepAlives = true
	l.waitGatewayEndpoints()
	l.waitFileAuthorization([]*http.Client{normal}, admin)
	l.request(normal, kindUser{}, "GET", recoveryPublicFile, "", nil, 404)
	l.request(normal, admin, "GET", recoveryReviewFile, "", nil, 404)
	t.Log("PASS: real Content outage denies both file methods, every Asset caller recovers independently, final strict 404 through each pinned caller and normal NodePort")
}
