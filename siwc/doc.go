// Package siwc is a small client for OpenAI's official "Sign in with ChatGPT"
// (SiwC) token-sharing flow for open-source / locally run apps. It lets ARTEX
// run on the user's ChatGPT plan (Plus/Pro) usage instead of an OpenAI API key.
//
// The package implements only the documented flow and nothing else:
//
//   - OIDC authorization-code + PKCE (S256) against auth.openai.com, first as the
//     dynamic client "dynamic_agent_client" (the callback returns an issued
//     oaiapp_... client id that is saved and reused — it is not a secret and no
//     client secret or API key is involved), then as the saved client;
//   - code exchange, ID-token verification (RS256 signature against the issuer's
//     JWKS, issuer, audience = issued client id, expiry, nonce);
//   - refresh-token rotation, revocation on sign-out;
//   - the granted-scope check: inference may only run when
//     chatgpt.tokens.use.direct was granted.
//
// It never reads Codex CLI credentials, never reuses another app's client id and
// never talks to ChatGPT backend-api endpoints. Tokens are secrets: nothing in
// this package logs them, and the error types never carry them.
//
// Specification (source of truth):
//
//	https://developers.openai.com/cookbook/articles/sign-in-with-chatgpt
//	https://developers.openai.com/siwc/token-sharing-open-source/sign-in
//	https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions
//	https://developers.openai.com/siwc/token-sharing-open-source/token-reference
//	https://developers.openai.com/siwc/token-sharing-open-source/errors-and-recovery
package siwc
