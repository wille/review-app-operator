package webhook

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	reviewapps "github.com/wille/review-app-operator/api/v1alpha1"
)

// GitHubActionsIssuer is the issuer of the ID tokens of GitHub Actions workflows
const GitHubActionsIssuer = "https://token.actions.githubusercontent.com"

// IDTokenVerifier verifies the GitHub Actions ID tokens that deploys can authenticate with
// instead of the webhook secret
type IDTokenVerifier struct {
	// Issuer is the issuer of the ID tokens, which is GitHubActionsIssuer, or the issuer of a
	// GitHub Enterprise
	Issuer string

	// Audience is the audience that the action requests the ID tokens for
	Audience string

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
}

// githubClaims are the claims of a GitHub Actions ID token that authorize a deploy
type githubClaims struct {
	// Repository is the repository of the workflow, like octo-org/octo-repo
	Repository string `json:"repository"`

	// EventName is the event that triggered the workflow
	EventName string `json:"event_name"`

	// HeadRef is the branch of the pull request
	HeadRef string `json:"head_ref"`

	// BaseRef is the branch that the pull request is merged into
	BaseRef string `json:"base_ref"`

	// Ref is refs/pull/<number>/merge for pull request workflows, and the base branch, like
	// refs/heads/main, once the pull request is merged
	Ref string `json:"ref"`
}

// discoveryTimeout limits how long the webhook waits for the issuer to describe its keys
const discoveryTimeout = 10 * time.Second

// getVerifier discovers the keys of the issuer on the first deploy with an ID token, so that the
// webhook starts without reaching the issuer, and tries again after the issuer can't be reached
func (v *IDTokenVerifier) getVerifier() (*oidc.IDTokenVerifier, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.verifier != nil {
		return v.verifier, nil
	}

	// The keys are fetched with this context later on, so it must not be canceled
	ctx := oidc.ClientContext(context.Background(), &http.Client{Timeout: discoveryTimeout})
	provider, err := oidc.NewProvider(ctx, v.Issuer)
	if err != nil {
		return nil, fmt.Errorf("discovering the ID token issuer %s: %w", v.Issuer, err)
	}

	v.verifier = provider.Verifier(&oidc.Config{ClientID: v.Audience})
	return v.verifier, nil
}

// verify returns the claims of an ID token that was issued for the audience, and hasn't expired
func (v *IDTokenVerifier) verify(ctx context.Context, rawToken string) (githubClaims, error) {
	verifier, err := v.getVerifier()
	if err != nil {
		return githubClaims{}, err
	}

	token, err := verifier.Verify(ctx, rawToken)
	if err != nil {
		return githubClaims{}, err
	}

	var claims githubClaims
	if err := token.Claims(&claims); err != nil {
		return githubClaims{}, err
	}

	return claims, nil
}

// errForbidden is returned when an ID token doesn't allow a deploy
var errForbidden = errors.New("forbidden")

// authorizeIDToken returns an error unless the ID token is from the pull request workflow of a
// repository that the ReviewAppConfig allows, for the branch and pull request of the webhook.
// It returns the number of the pull request that the token proves, which is 0 for a merged pull
// request, whose token is for the branch that it was merged into instead of the pull request
func authorizeIDToken(reviewApp *reviewapps.ReviewAppConfig, claims githubClaims, webhook WebhookBody, method string) (int, error) {
	allowed := slices.ContainsFunc(reviewApp.Spec.Repositories, func(repository string) bool {
		// GitHub repository names are case insensitive
		return strings.EqualFold(repository, claims.Repository)
	})
	if !allowed {
		return 0, fmt.Errorf("%w: the ReviewAppConfig %s does not allow deploys from %s, which can be added to its spec.repositories",
			errForbidden, reviewApp.Name, claims.Repository)
	}

	if claims.EventName != "pull_request" {
		return 0, fmt.Errorf("%w: the ID token is from a %s workflow, not a pull_request workflow", errForbidden, claims.EventName)
	}

	if claims.HeadRef != webhook.BranchName {
		return 0, fmt.Errorf("%w: the ID token is from the branch %q, not %q", errForbidden, claims.HeadRef, webhook.BranchName)
	}

	if claims.Ref == "refs/pull/"+strconv.Itoa(webhook.PullRequestNumber)+"/merge" {
		return webhook.PullRequestNumber, nil
	}

	// Closing a merged pull request deletes its review app with a token for the base branch, which
	// can't tell the pull request, so it deletes the review app of the branch of any pull request
	if claims.BaseRef != "" && claims.Ref == "refs/heads/"+claims.BaseRef {
		if method == http.MethodDelete {
			return 0, nil
		}
		return 0, fmt.Errorf("%w: the ID token is from the merged pull request of the branch %q, which can only delete its review app",
			errForbidden, claims.HeadRef)
	}

	return 0, fmt.Errorf("%w: the ID token is from %s, not pull request %d", errForbidden, claims.Ref, webhook.PullRequestNumber)
}
