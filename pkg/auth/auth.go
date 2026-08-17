/*
 * Copyright 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package auth reads the caller's identity from the access token and enforces
// the `developer` realm role.
//
// It does not validate the token. Signature, expiry and audience are checked
// centrally by the platform API gateway, so a request that reaches this
// process is already authenticated and re-checking it in every service would
// duplicate the gateway. The token is parsed unverified, via the shared
// service-commons type, purely to read `sub` and `realm_access`.
//
// Two consequences worth keeping in mind:
//
//   - The gateway must be the only route to this service. Cluster-internal
//     callers that bypass it are not authenticated by anything. SPEC §5.6
//     item 2 and M10 cover the NetworkPolicy that enforces this, and the
//     JupyterHub singleuser pods running developer code make it concrete.
//   - Role checking stays here. The gateway authenticates; which realm role
//     ODE requires is ODE's own authorisation decision (SPEC D5).
package auth

import (
	"errors"
	"net/http"

	servicejwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
	"github.com/gin-gonic/gin"
)

// Token is the platform's shared claims type. Aliased rather than wrapped so a
// token read here can be handed to any other SENERGY library unchanged.
type Token = servicejwt.Token

var (
	ErrMissingAuthToken = servicejwt.ErrMissingAuthToken
	ErrInvalidAuth      = servicejwt.ErrInvalidAuth
)

const contextKey = "ode/token"

// Middleware reads the token and enforces the required realm role. A missing
// or unparseable token is 401; a well-formed token without the role is 403.
// Keeping the two apart matters to the SPA: it retries a 401 by refreshing,
// whereas a 403 is final and must be shown to the user.
func Middleware(requiredRole string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := servicejwt.GetParsedToken(c.Request)
		if err != nil {
			if errors.Is(err, ErrMissingAuthToken) {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing auth token"})
				return
			}
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid auth token"})
			return
		}
		// ParseUnverified does not run Valid(), so a token with no subject
		// would otherwise arrive here as an anonymous caller.
		if err := token.Valid(); err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid auth token"})
			return
		}
		if requiredRole != "" && !token.HasRole(requiredRole) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error":         "missing required realm role",
				"required_role": requiredRole,
			})
			return
		}
		c.Set(contextKey, token)
		c.Next()
	}
}

// FromContext returns the token stored by Middleware.
func FromContext(c *gin.Context) (Token, bool) {
	v, exists := c.Get(contextKey)
	if !exists {
		return Token{}, false
	}
	token, ok := v.(Token)
	return token, ok
}

// MustFromContext is for handlers that are unreachable without Middleware.
func MustFromContext(c *gin.Context) Token {
	token, ok := FromContext(c)
	if !ok {
		panic("auth: no token in context - handler is not behind auth.Middleware")
	}
	return token
}

// Bearer returns the caller's token as an Authorization header value, for
// reading from the platform on their behalf (SPEC D5, §3.1 step 3).
func Bearer(c *gin.Context) string {
	token := MustFromContext(c)
	return token.Jwt()
}
