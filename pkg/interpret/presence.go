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

package interpret

import (
	"sync"

	"github.com/SENERGY-Platform/operator-development-environment/pkg/chat"
)

// presence is which developers have a live credential right now.
//
// This is how the token problem is solved rather than worked around. ODE cannot
// mint a token for a developer and must not use a service account for their data
// (§3.1 item 3), so the only credential that may run an interpretation turn is one
// they are presenting themselves. pkg/api registers a connection's *token source*
// here — not a copy of the token — for as long as the WebSocket is up, and the SPA
// refreshes that source every time it renews (see api.sessionToken). So what is
// registered stays valid for the life of the connection rather than for the life
// of the first token, which is what lets a turn started now still read the platform
// twelve minutes into a tool loop.
//
// Several connections per developer is the normal case — two tabs — so the sources
// are a set and any of them serves. Removing one leaves the others; removing the
// last one means the developer is gone and the pending runs wait.
type presence struct {
	mux     sync.RWMutex
	next    int
	sources map[string]map[int]chat.TokenSource
}

func newPresence() *presence {
	return &presence{sources: map[string]map[int]chat.TokenSource{}}
}

// add registers a live credential and returns the function that withdraws it.
func (p *presence) add(userSub string, source chat.TokenSource) func() {
	if userSub == "" || source == nil {
		return func() {}
	}
	p.mux.Lock()
	id := p.next
	p.next++
	if p.sources[userSub] == nil {
		p.sources[userSub] = map[int]chat.TokenSource{}
	}
	p.sources[userSub][id] = source
	p.mux.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			p.mux.Lock()
			defer p.mux.Unlock()
			delete(p.sources[userSub], id)
			if len(p.sources[userSub]) == 0 {
				delete(p.sources, userSub)
			}
		})
	}
}

// token is a live credential for this developer, if there is one.
//
// The source is returned rather than the string it currently yields, so the turn
// that uses it reads the *current* token on every tool call — the whole point of
// chat.TokenSource. Handing back a string here would reintroduce the expiry
// problem the type exists to solve.
func (p *presence) token(userSub string) (chat.TokenSource, bool) {
	p.mux.RLock()
	defer p.mux.RUnlock()
	for _, source := range p.sources[userSub] {
		return source, true
	}
	return nil, false
}

// connected is every developer with a live credential, so a retry pass knows whose
// pending runs are worth trying.
func (p *presence) connected() []string {
	p.mux.RLock()
	defer p.mux.RUnlock()
	out := make([]string, 0, len(p.sources))
	for userSub := range p.sources {
		out = append(out, userSub)
	}
	return out
}
