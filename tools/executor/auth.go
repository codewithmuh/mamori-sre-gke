package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// errUnauthenticated means the caller is not the agent; any other error means the check
// itself could not run.
var errUnauthenticated = errors.New("unauthenticated")

type authenticator interface {
	Authenticate(ctx context.Context, bearer string) (string, error)
}

// tokenReviewAuth accepts a projected service account token with our audience, checked by
// the API server (TokenReview). The agent sends its own short-lived token, so there is no
// shared secret to create, store or rotate. NetworkPolicy already limits who can connect;
// this also proves which workload is calling.
type tokenReviewAuth struct {
	client   kubernetes.Interface
	audience string
	allowed  []string // e.g. system:serviceaccount:mamori-agent:agent
	ttl      time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]cachedReview
}

type cachedReview struct {
	user    string
	expires time.Time
}

const maxCachedTokens = 64

func newTokenReviewAuth(client kubernetes.Interface, audience string, allowed []string, now func() time.Time) *tokenReviewAuth {
	return &tokenReviewAuth{client: client, audience: audience, allowed: allowed, ttl: time.Minute, now: now,
		cache: map[[32]byte]cachedReview{}}
}

// Authenticate returns the caller's user name, errUnauthenticated, or an error from the review.
func (a *tokenReviewAuth) Authenticate(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", fmt.Errorf("%w: no bearer token", errUnauthenticated)
	}
	key := sha256.Sum256([]byte(token))
	now := a.now()
	a.mu.Lock()
	if c, ok := a.cache[key]; ok && now.Before(c.expires) {
		a.mu.Unlock()
		return c.user, nil
	}
	a.mu.Unlock()

	review, err := a.client.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token, Audiences: []string{a.audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("token review: %w", err)
	}
	st := review.Status
	switch {
	case !st.Authenticated:
		return "", fmt.Errorf("%w: token rejected: %s", errUnauthenticated, st.Error)
	case !slices.Contains(st.Audiences, a.audience):
		return "", fmt.Errorf("%w: token is not for audience %q", errUnauthenticated, a.audience)
	case !slices.Contains(a.allowed, st.User.Username):
		return "", fmt.Errorf("%w: %s may not call the executor", errUnauthenticated, st.User.Username)
	}

	a.mu.Lock()
	if len(a.cache) >= maxCachedTokens {
		clear(a.cache)
	}
	a.cache[key] = cachedReview{user: st.User.Username, expires: now.Add(a.ttl)}
	a.mu.Unlock()
	return st.User.Username, nil
}

// noAuth is for local runs only (AUTH_MODE=none); the executor logs a warning at start.
type noAuth struct{}

// Authenticate accepts everyone.
func (noAuth) Authenticate(context.Context, string) (string, error) { return "anonymous", nil }
