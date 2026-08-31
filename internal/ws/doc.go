// Package ws is the WebSocket hub: topics, per-topic authorization and fan-out.
//
// Specification: 14. It is a transport and holds no game-specific knowledge (ADR-042) —
// framing, line reassembly and pattern matching live in internal/instance, next to the
// measured log grammar they depend on, and the adapters that turn a log line into a wire
// message live in internal/api. This package imports neither.
//
// Nor is it a system of record (14 §9): job state is in job_runs, instance state in
// instances, and the audit trail in audit_log. It interprets no roles — it calls Can, per
// topic, on every subscribe.
package ws
