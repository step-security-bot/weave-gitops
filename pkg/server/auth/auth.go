package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"

	"github.com/sethvargo/go-limiter/httplimit"
	"github.com/sethvargo/go-limiter/memorystore"

	"github.com/weaveworks/weave-gitops/core/logger"
	"github.com/weaveworks/weave-gitops/pkg/featureflags"
)

const (
	// StateCookieName is the name of the cookie that holds state during auth flow.
	StateCookieName = "state"
	// IDTokenCookieName is the name of the cookie that holds the ID Token once
	// the user has authenticated successfully with the OIDC Provider.
	IDTokenCookieName = "id_token"
	// AccessTokenCookieName is the name of the cookie that holds the access token once
	// the user has authenticated successfully with the OIDC Provider. It's used for further
	// resource requests from the provider.
	AccessTokenCookieName = "access_token"
	// RefreshTokenCookieName is the name of the cookie that holds the refresh token once
	// the user has authenticated successfully with the OIDC Provider. It's used to refresh
	// the id and access tokens once expired.
	RefreshTokenCookieName = "refresh_token"
	// AuthorizationTokenHeaderName is the name of the header that holds the bearer token
	// used for token passthrough authentication.
	AuthorizationTokenHeaderName = "Authorization"
	// ScopeEmail is the "email" scope
	ScopeEmail = "email"
	// ScopeGroups is the "groups" scope
	ScopeGroups = "groups"
)

// RegisterAuthServer registers the /callback route under a specified prefix.
// This route is called by the OIDC Provider in order to pass back state after
// the authentication flow completes.
func RegisterAuthServer(mux *http.ServeMux, prefix string, srv *AuthServer, loginRequestRateLimit uint64) error {
	store, err := memorystore.New(&memorystore.Config{
		Tokens: loginRequestRateLimit,
	})
	if err != nil {
		return err
	}

	middleware, err := httplimit.NewMiddleware(store, httplimit.IPKeyFunc())
	if err != nil {
		return err
	}

	mux.Handle(prefix, srv.OAuth2Flow())
	mux.HandleFunc(prefix+"/callback", srv.Callback)
	mux.Handle(prefix+"/sign_in", middleware.Handle(srv.SignIn()))
	mux.HandleFunc(prefix+"/userinfo", srv.UserInfo)
	mux.HandleFunc(prefix+"/refresh", srv.RefreshHandler)
	mux.HandleFunc(prefix+"/logout", srv.Logout)

	return nil
}

type principalCtxKey struct{}

// Principal gets the principal from the context.
func Principal(ctx context.Context) *UserPrincipal {
	principal, ok := ctx.Value(principalCtxKey{}).(*UserPrincipal)
	if ok {
		return principal
	}

	return nil
}

// UserPrincipal is a simple model for the user, including their ID and Groups.
type UserPrincipal struct {
	ID     string   `json:"id"`
	Groups []string `json:"groups"`
	token  *string  `json:"-"`
}

// Token returns the private access token for this principal.
func (p UserPrincipal) Token() string {
	if p.token == nil {
		return ""
	}

	return *p.token
}

// SetToken allows setting of the private access token.
func (p *UserPrincipal) SetToken(t string) {
	p.token = &t
}

// String returns the Principal ID and Groups as a string.
func (p *UserPrincipal) String() string {
	return fmt.Sprintf("id=%q groups=%v", p.ID, p.Groups)
}

// Hash returns a unique string using user id,token and groups.
func (p *UserPrincipal) Hash() string {
	hash := sha256.Sum224([]byte(fmt.Sprintf("%s/%s/%v", p.ID, p.Token(), p.Groups)))
	return hex.EncodeToString(hash[:])
}

func (p *UserPrincipal) Valid() bool {
	if p.ID == "" && p.Token() == "" {
		return false
	}

	return true
}

// NewUserPrincipal creates a new Principal and applies the configuration options.
func NewUserPrincipal(opts ...func(*UserPrincipal)) *UserPrincipal {
	p := &UserPrincipal{}
	for _, o := range opts {
		o(p)
	}

	return p
}

// Token is an option func for NewUserPrincipal that sets the token.
func Token(tok string) func(*UserPrincipal) {
	return func(p *UserPrincipal) {
		p.SetToken(tok)
	}
}

// Groups is an option func for NewUserPrincipal that configures the groups.
func Groups(groups []string) func(*UserPrincipal) {
	return func(p *UserPrincipal) {
		p.Groups = groups
	}
}

// ID is an option func for NewUserPrincipal that configures the groups.
func ID(id string) func(*UserPrincipal) {
	return func(p *UserPrincipal) {
		p.ID = id
	}
}

// WithPrincipal sets the principal into the context.
func WithPrincipal(ctx context.Context, p *UserPrincipal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// WithAPIAuth middleware adds auth validation to API handlers.
//
// Unauthorized requests will be denied with a 401 status code.
func WithAPIAuth(next http.Handler, srv *AuthServer, publicRoutes []string, sm SessionManager) http.Handler {
	multi := MultiAuthPrincipal{Log: srv.Log, Getters: []PrincipalGetter{}}

	// FIXME: currently the order must be OIDC last, or it'll "shadow" the other
	// methods so they don't work.
	methods := []AuthMethod{UserAccount, TokenPassthrough, OIDC, Anonymous}
	for _, method := range methods {
		enabled, ok := srv.authMethods[method]
		if !ok {
			continue
		}

		if !enabled {
			srv.Log.V(logger.LogLevelWarn).Info("Disabled AuthMethod encountered", "AuthMethod", method.String())
			continue // in theory nothing should ever be set and not enabled but in case it is
		}

		switch method {
		case OIDC:
			if srv.oidcEnabled() {
				// OIDC tokens may be passed by token or cookie
				multi.Getters = append(multi.Getters, NewJWTAuthorizationHeaderPrincipalGetter(srv.Log, srv.verifier(), srv.OIDCConfig.ClaimsConfig))

				if srv.oidcPassthroughEnabled() {
					srv.Log.V(logger.LogLevelDebug).Info("JWT Token Passthrough Enabled")
					multi.Getters = append(multi.Getters, NewJWTPassthroughCookiePrincipalGetter(srv.Log, srv.verifier(), IDTokenCookieName, sm))
				} else {
					multi.Getters = append(multi.Getters, NewJWTCookiePrincipalGetter(srv.Log, srv.verifier(), srv.OIDCConfig.ClaimsConfig, IDTokenCookieName, sm))
				}
			}

		case UserAccount:
			if featureflags.IsSet(FeatureFlagClusterUser) {
				adminAuth := NewJWTAdminCookiePrincipalGetter(srv.Log, srv.tokenSignerVerifier, IDTokenCookieName, sm)

				multi.Getters = append(multi.Getters, adminAuth)
			}

		case TokenPassthrough:
			tokenAuth := NewBearerTokenPassthroughPrincipalGetter(srv.Log, nil, AuthorizationTokenHeaderName, srv.kubernetesClient)
			multi.Getters = append(multi.Getters, tokenAuth)

		case Anonymous:
			multi.Getters = []PrincipalGetter{NewAnonymousPrincipalGetter(srv.Log, srv.noAuthUser)}
		}
	}

	return &authenticatedMiddleware{
		next:            next,
		srv:             srv,
		publicRoutes:    publicRoutes,
		principalGetter: multi,
		sm:              sm,
	}
}

type authenticatedMiddleware struct {
	srv             *AuthServer
	publicRoutes    []string
	next            http.Handler
	principalGetter PrincipalGetter
	sm              SessionManager
}

func (a *authenticatedMiddleware) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	if IsPublicRoute(r.URL, a.publicRoutes) {
		a.next.ServeHTTP(rw, r)
		return
	}

	principal, err := a.principalGetter.Principal(r)

	if err != nil || principal == nil {
		JSONError(a.srv.Log, rw, "Authentication required", http.StatusUnauthorized)
		return
	}

	a.next.ServeHTTP(rw, r.Clone(WithPrincipal(r.Context(), principal)))
}

func generateNonce() (string, error) {
	b := make([]byte, 32)

	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(b), nil
}

func IsPublicRoute(u *url.URL, publicRoutes []string) bool {
	for _, pr := range publicRoutes {
		if u.Path == pr {
			return true
		}
	}

	return false
}
