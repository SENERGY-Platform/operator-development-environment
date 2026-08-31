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

// Package permissions asks permissions-v2 whether the developer may execute a
// set of resources.
//
// It satisfies access.Checker from the flow engine's lib/access, which carries
// the rule this implements the transport for. The rule is shared and the client
// is not, deliberately: lib/access holds only the part that can drift -- which
// ids an input topic names, which are exempt, what happens to one that cannot be
// resolved -- while a permissions-v2 client pulls some four hundred packages
// behind it, which every consumer of that module would otherwise compile.
package permissions

import (
	"github.com/SENERGY-Platform/permissions-v2/pkg/client"
)

type Client struct {
	c client.Client
}

func New(baseURL string) *Client {
	return &Client{c: client.New(baseURL)}
}

// UserHasExecuteAccess reports whether the token holder may execute every one of
// the given ids. All of them, not any: the caller is about to read all of them,
// so a partial answer is a denial.
func (p *Client) UserHasExecuteAccess(resource string, ids []string, authorization string) (bool, error) {
	response, err, _ := p.c.CheckMultiplePermissions(authorization, resource, ids, client.Execute)
	if err != nil {
		return false, err
	}
	// Ranged over the ids rather than over the response: an id the service did not
	// answer for is simply absent from the map, and ranging over what came back
	// would take silence for a yes.
	for _, id := range ids {
		if !response[id] {
			return false, nil
		}
	}
	return true, nil
}
