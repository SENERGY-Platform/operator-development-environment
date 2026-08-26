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

package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/swaggo/swag"

	// The generated specification. It *is* committed, deliberately: the spec has to
	// be readable from the repository without a build, for a human and for a tool
	// alike. `go generate ./...` rewrites it, the Dockerfile runs that before
	// building, and CI regenerates and fails on a diff — which is the guarantee
	// that makes committing a generated artifact safe rather than a lie.
	_ "github.com/SENERGY-Platform/operator-development-environment/docs"
)

//go:generate go tool swag init -o ../../docs --parseDependency -d .. -g api/api.go

// handleDoc serves the OpenAPI specification for this build.
//
// Unauthenticated, like /health: the shape of the API is not privileged, and the
// platform's developer-swagger-api has to be able to collect it without holding a
// developer's token.
//
// @Summary		This specification
// @Description	The OpenAPI document for this build. Unauthenticated, because the shape
// @Description	of the API is not privileged and the platform's developer-swagger-api
// @Description	has to be able to collect it without holding a developer's token.
// @Tags			meta
// @Produce		json
// @Success		200	{object}	map[string]interface{}
// @Failure		500	{object}	map[string]string	"the embedded specification could not be read"
// @Router			/doc [get]
func handleDoc() gin.HandlerFunc {
	return func(c *gin.Context) {
		doc, err := swag.ReadDoc()
		if err != nil {
			slog.ErrorContext(c, "the embedded openapi specification could not be read",
				"error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "specification unavailable"})
			return
		}
		// The empty host is removed rather than left in place so that
		// developer-swagger-api can substitute its own. It matches on the literal
		// text, so the formatting here is load-bearing — see the same workaround in
		// timescale-wrapper's pkg/api/doc.go.
		doc = strings.Replace(doc, `"host": "",`, "", 1)
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.String(http.StatusOK, doc)
	}
}
