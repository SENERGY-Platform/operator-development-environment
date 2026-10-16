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
	"net/url"

	"github.com/gin-gonic/gin"
)

const redactedValue = "REDACTED"

// redactAccessTokenForLog replaces the access_token query value on the request URL once the
// handler has returned. The access logger registered before it reads the URL after its own
// Next, so it sees the redacted copy while handlers still read the original.
func redactAccessTokenForLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			c.Request.URL = redactAccessToken(c.Request.URL)
		}()
		c.Next()
	}
}

// redactAccessToken returns a copy of u without the access_token value. A query that does not
// parse is dropped entirely, since the token cannot be located in it.
func redactAccessToken(u *url.URL) *url.URL {
	redacted := *u
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		redacted.RawQuery = redactedValue
		return &redacted
	}
	if !query.Has("access_token") {
		return &redacted
	}
	query.Set("access_token", redactedValue)
	redacted.RawQuery = query.Encode()
	return &redacted
}
