// Package nativeassettransport carries the immutable signed asset manifest for
// one protected model load from a Go parent to one inherited native child over
// an anonymous Unix socketpair. The child can request only required manifest
// assets in canonical order. The package exposes no TCP/HTTP listener,
// filesystem path, random access, complete plaintext cache, argv secret or
// environment secret.
package nativeassettransport
