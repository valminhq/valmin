// Package api holds the HTTP handlers and their DTOs. Each handler calls authz.Can in
// its own body (ADR-037).
//
// Specification: 11, 04 §3.
package api
