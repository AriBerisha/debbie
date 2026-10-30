package config

import "regexp"

// tokenFormats are the shapes of common API tokens and keys (SPEC §19.4):
// OpenAI and Anthropic, GitHub, GitLab, AWS access keys, Slack, Google.
var tokenFormats = regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}|` +
	`gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|glpat-[A-Za-z0-9_-]{20,}|` +
	`(AKIA|ASIA)[0-9A-Z]{16}|xox[abposr]-[A-Za-z0-9-]{10,}|AIza[0-9A-Za-z_-]{35}`)

// LooksLikeToken reports whether s holds text shaped like a common API
// token. Text leaving for the web that does always asks first, even in
// YOLO (SPEC §19.4).
func LooksLikeToken(s string) bool { return tokenFormats.MatchString(s) }
