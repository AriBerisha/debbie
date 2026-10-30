package prompt

import (
	"embed"
	"strings"
)

//go:embed tools/*.md
var toolFiles embed.FS

// Tool returns the description of a tool, from tools/<name>.md.
func Tool(name string) string {
	data, err := toolFiles.ReadFile("tools/" + name + ".md")
	if err != nil {
		panic("no description for tool " + name)
	}
	return strings.TrimSpace(string(data))
}

// ShellTool returns the shell tool's description for a shell: its display
// name, such as "cmd.exe", and its kind: cmd, powershell, bash or sh.
func ShellTool(shellName, kind string) string {
	notes, err := toolFiles.ReadFile("tools/shell_" + kind + ".md")
	if err != nil {
		notes = nil
	}
	return strings.NewReplacer("{{shell}}", shellName, "{{notes}}", strings.TrimSpace(string(notes))).Replace(Tool("shell"))
}
