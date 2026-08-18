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

// Package identifiers mints the ids ODE puts in URLs.
//
// Random rather than sequential, and that is a security property rather than a
// style preference: a chat session id appears in a URL and in an MCP header, and
// ownership is checked on every read — but a sequential id would let anyone
// enumerate how many sessions exist and probe for neighbours. There is nothing to
// probe for here.
package identifiers

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// Source mints ids. An interface at the call sites (tools.IDs, chat.IDs) so tests
// get deterministic ones.
type Source struct{}

func New() *Source { return &Source{} }

// idBytes is 16 bytes, 128 bits. Enough that a collision is not worth handling
// and that guessing one is not worth attempting.
const idBytes = 16

// NewID returns a URL-safe identifier.
//
// base32 without padding rather than base64: the id travels in URL path segments
// and in an HTTP header, and base32's alphabet needs no escaping in either.
// Lowercased because the ids are read aloud and typed by hand during development.
func (s *Source) NewID() string {
	buffer := make([]byte, idBytes)
	// rand.Read from crypto/rand is documented never to return an error since Go
	// 1.24 — it panics internally if the system source fails, which is the correct
	// behaviour for a process that cannot mint unguessable ids.
	_, _ = rand.Read(buffer)
	return strings.ToLower(
		base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buffer))
}
