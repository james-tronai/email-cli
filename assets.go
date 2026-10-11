// Package emailcli holds the files that ship inside the binary, so that
// `email hermes files` and `email hermes install` write exactly what was
// released: the agent skill, the Hermes integration and the policy templates.
package emailcli

import "embed"

// EmailSkill is the agent skill (skills/email/SKILL.md).
//
//go:embed skills/email/SKILL.md
var EmailSkill []byte

// HermesRouteScript is the Hermes webhook route script for `email watch`.
//
//go:embed contrib/hermes/email-summary.py
var HermesRouteScript []byte

// HermesCronJob is the email triage cron job for `email hermes seed-cron`.
//
//go:embed contrib/hermes/cron-email-triage.json
var HermesCronJob []byte

// PolicyTemplates holds docs/policy/<name>.toml.
//
//go:embed docs/policy/*.toml
var PolicyTemplates embed.FS
