// Package api — user-facing HTTP handlers.
//
// Single-user mode: the upstream app_user provisioning endpoint
// (POST /v1/users) and the X-Fastclaw-End-User / `user`-body identity
// switch have been removed. The platform now serves exactly one owner.
package api
