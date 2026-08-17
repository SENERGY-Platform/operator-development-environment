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

package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// mintToken builds a token with an arbitrary signature. That is deliberate:
// the gateway checks signatures, this package only reads claims, and a test
// that signed properly would imply a verification step that does not exist.
func mintToken(claims map[string]any) string {
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": "gateway"}
	return segment(header) + "." + segment(claims) + "." + base64.RawURLEncoding.EncodeToString([]byte("signature-checked-at-the-gateway"))
}

func segment(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func developerClaims() map[string]any {
	return map[string]any{
		"sub":                "user-123",
		"preferred_username": "dev",
		"email":              "dev@example.org",
		"realm_access":       map[string]any{"roles": []string{"developer", "offline_access"}},
	}
}

func routerUnderTest(requiredRole string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/protected", Middleware(requiredRole), func(c *gin.Context) {
		token := MustFromContext(c)
		c.JSON(http.StatusOK, gin.H{
			"sub":    token.Sub,
			"bearer": Bearer(c),
		})
	})
	return r
}

func call(r *gin.Engine, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestMiddlewareAllowsADeveloperThrough(t *testing.T) {
	w := call(routerUnderTest("developer"), "Bearer "+mintToken(developerClaims()))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "user-123") {
		t.Errorf("body = %s, want the subject from the token", w.Body.String())
	}
}

func TestMiddlewareAnswers401WithoutAToken(t *testing.T) {
	if w := call(routerUnderTest("developer"), ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestMiddlewareAnswers401ForAnUnparseableToken(t *testing.T) {
	if w := call(routerUnderTest("developer"), "Bearer not-a-jwt"); w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// ParseUnverified does not run Valid(), so without the explicit check a token
// carrying no subject would arrive as an anonymous caller with a valid shape.
func TestMiddlewareAnswers401ForATokenWithoutASubject(t *testing.T) {
	claims := developerClaims()
	delete(claims, "sub")

	if w := call(routerUnderTest("developer"), "Bearer "+mintToken(claims)); w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// The distinction matters to the SPA: 401 means refresh and retry, 403 is final.
func TestMiddlewareAnswers403WhenTheDeveloperRoleIsMissing(t *testing.T) {
	claims := developerClaims()
	claims["realm_access"] = map[string]any{"roles": []string{"offline_access"}}

	w := call(routerUnderTest("developer"), "Bearer "+mintToken(claims))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "developer") {
		t.Errorf("body = %s, want it to name the required role", w.Body.String())
	}
}

func TestMiddlewareAnswers403WhenThereAreNoRolesAtAll(t *testing.T) {
	claims := developerClaims()
	delete(claims, "realm_access")

	if w := call(routerUnderTest("developer"), "Bearer "+mintToken(claims)); w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestAnEmptyRequiredRoleAdmitsAnyAuthenticatedCaller(t *testing.T) {
	claims := developerClaims()
	claims["realm_access"] = map[string]any{"roles": []string{"offline_access"}}

	if w := call(routerUnderTest(""), "Bearer "+mintToken(claims)); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// SPEC §3.1 step 3: the caller's token is forwarded verbatim to the platform,
// so it has to survive parsing intact.
func TestBearerReturnsTheTokenAsPresented(t *testing.T) {
	raw := mintToken(developerClaims())
	w := call(routerUnderTest("developer"), "Bearer "+raw)

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body["bearer"], raw) {
		t.Errorf("Bearer() = %q, want it to carry the presented token", body["bearer"])
	}
	if !strings.HasPrefix(body["bearer"], "Bearer ") {
		t.Errorf("Bearer() = %q, want a Bearer-prefixed header value", body["bearer"])
	}
}

func TestExpiredTokensAreAcceptedBecauseTheGatewayChecksThem(t *testing.T) {
	claims := developerClaims()
	claims["exp"] = 1 // 1970, long expired

	// This documents the trust boundary rather than endorsing it: expiry is
	// the gateway's job, and this service is only reachable through it once
	// the M10 NetworkPolicy is in place.
	if w := call(routerUnderTest("developer"), "Bearer "+mintToken(claims)); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: expiry is validated at the gateway, not here", w.Code)
	}
}

func TestFromContextReportsAbsenceRatherThanPanicking(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if _, ok := FromContext(c); ok {
		t.Error("FromContext reported a token on a context that has none")
	}
}
