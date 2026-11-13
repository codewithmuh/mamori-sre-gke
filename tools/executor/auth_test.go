package main

import (
	"context"
	"errors"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const agentUser = "system:serviceaccount:mamori-agent:agent"

// reviewer answers TokenReviews from a table of token -> (user, audiences).
func reviewer(t *testing.T, calls *int) *fake.Clientset {
	t.Helper()
	client := fake.NewClientset()
	client.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		*calls++
		tr := a.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		if len(tr.Spec.Audiences) != 1 || tr.Spec.Audiences[0] != "executor" {
			t.Errorf("review asked for audiences %v", tr.Spec.Audiences)
		}
		out := tr.DeepCopy()
		switch tr.Spec.Token {
		case "agent-token":
			out.Status = authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: []string{"executor"},
				User: authenticationv1.UserInfo{Username: agentUser}}
		case "other-sa":
			out.Status = authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: []string{"executor"},
				User: authenticationv1.UserInfo{Username: "system:serviceaccount:boutique:default"}}
		case "wrong-audience":
			out.Status = authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: []string{"https://kubernetes.default.svc"},
				User: authenticationv1.UserInfo{Username: agentUser}}
		case "api-down":
			return true, nil, errors.New("connection refused")
		default:
			out.Status = authenticationv1.TokenReviewStatus{Authenticated: false, Error: "invalid bearer token"}
		}
		return true, out, nil
	})
	return client
}

func TestTokenReviewAuth(t *testing.T) {
	calls := 0
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	auth := newTokenReviewAuth(reviewer(t, &calls), "executor", []string{agentUser}, func() time.Time { return now })
	ctx := context.Background()

	user, err := auth.Authenticate(ctx, "agent-token")
	if err != nil || user != agentUser {
		t.Fatalf("agent token: %q, %v", user, err)
	}
	if _, err := auth.Authenticate(ctx, "agent-token"); err != nil || calls != 1 {
		t.Fatalf("second call should be cached: calls=%d err=%v", calls, err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := auth.Authenticate(ctx, "agent-token"); err != nil || calls != 2 {
		t.Fatalf("cache should expire: calls=%d err=%v", calls, err)
	}

	for _, tok := range []string{"", "other-sa", "wrong-audience", "forged"} {
		if _, err := auth.Authenticate(ctx, tok); !errors.Is(err, errUnauthenticated) {
			t.Errorf("%q: want errUnauthenticated, got %v", tok, err)
		}
	}
	if _, err := auth.Authenticate(ctx, "api-down"); err == nil || errors.Is(err, errUnauthenticated) {
		t.Errorf("API failure must not look like a bad token: %v", err)
	}
}

func TestTokenCacheIsBounded(t *testing.T) {
	calls := 0
	auth := newTokenReviewAuth(reviewer(t, &calls), "executor", []string{agentUser}, time.Now)
	for range maxCachedTokens + 3 {
		auth.cache[[32]byte{byte(len(auth.cache))}] = cachedReview{}
		if len(auth.cache) >= maxCachedTokens {
			break
		}
	}
	if _, err := auth.Authenticate(context.Background(), "agent-token"); err != nil {
		t.Fatal(err)
	}
	if len(auth.cache) != 1 {
		t.Errorf("cache has %d entries after reset, want 1", len(auth.cache))
	}
}

func TestNoAuth(t *testing.T) {
	if u, err := (noAuth{}).Authenticate(context.Background(), ""); err != nil || u != "anonymous" {
		t.Fatalf("%q %v", u, err)
	}
}
