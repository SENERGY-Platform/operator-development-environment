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

package repo

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// §5.11 item 1 says the GitHub token is stored encrypted. This is that, and it is
// deliberately the whole of it: AES-256-GCM under a key from the deployment's
// configuration, nothing homemade.
//
// What it does and does not buy is worth being precise about, because a reader who
// overestimates it will make a worse decision later. It protects the token at
// rest — a database dump, a backup, a replica someone can read — and it does not
// protect it from ODE itself, which necessarily holds the key in order to push on
// the developer's behalf. The property that matters is the one it does have: the
// Keycloak session and the GitHub credential are separable, so a stolen token
// table is not a stolen set of repositories.

// Sealer encrypts a token for storage.
type Sealer struct {
	aead cipher.AEAD
}

// NewSealer builds a sealer from a base64-encoded 32-byte key.
//
// It fails rather than deriving a key from something weaker. A deployment that
// wants this package configures a key; one that does not gets no repo routes at
// all, which is the same shape as a missing timescale-wrapper elsewhere in ODE.
func NewSealer(key string) (*Sealer, error) {
	raw, err := decodeKey(key)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("config: github_token_key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("config: github_token_key: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// decodeKey accepts standard or URL-safe base64, with or without padding —
// `openssl rand -base64 32` and `head -c 32 /dev/urandom | base64` produce
// different spellings of the same thing and both are what an operator will type.
func decodeKey(key string) ([]byte, error) {
	if key == "" {
		return nil, errors.New("config: github_token_key is required to store a GitHub token")
	}
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if raw, err := encoding.DecodeString(key); err == nil {
			if len(raw) != 32 {
				return nil, fmt.Errorf(
					"config: github_token_key decodes to %d bytes, want 32 (generate one with "+
						"`openssl rand -base64 32`)", len(raw))
			}
			return raw, nil
		}
	}
	return nil, errors.New("config: github_token_key is not base64")
}

// Seal encrypts a token. The nonce is random per call and stored in front of the
// ciphertext, so sealing the same token twice does not produce the same row.
func (s *Sealer) Seal(plaintext string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("sealing the github token: %w", err)
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a token. A failure here is not recoverable and must not be
// papered over: it means the key changed or the row was tampered with, and the
// answer either way is that the developer reconnects.
func (s *Sealer) Open(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("the stored github token is not readable: %w", err)
	}
	if len(raw) < s.aead.NonceSize() {
		return "", errors.New("the stored github token is truncated")
	}
	nonce, ciphertext := raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():]
	plaintext, err := s.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("the stored github token could not be decrypted, so this "+
			"developer has to connect GitHub again: %w", err)
	}
	return string(plaintext), nil
}
