package config

import "strings"

// ParseApprovalMode reads an approval mode as the user types it, with the
// short names: "edit" for editor and "auto" for auto-edit.
func ParseApprovalMode(s string) (string, bool) {
	switch s = strings.ToLower(strings.TrimSpace(s)); s {
	case ApprovalPlan, ApprovalDefault, ApprovalEditor, ApprovalAutoEdit, ApprovalYolo:
		return s, true
	case "edit":
		return ApprovalEditor, true
	case "auto":
		return ApprovalAutoEdit, true
	}
	return "", false
}

// ApprovalModeNames lists the modes for messages.
const ApprovalModeNames = "plan, default, editor, auto (auto-edit) or yolo"
