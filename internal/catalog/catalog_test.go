package catalog

import (
	"strings"
	"testing"
)

func TestMetadataEnablesSkillsAndFunctionTools(t *testing.T) {
	metadata := Metadata("deepseek-v4-pro", "DeepSeek-V4-Pro")
	if metadata["include_skills_usage_instructions"] != true {
		t.Fatal("generated Codex model does not enable skill usage instructions")
	}
	if metadata["supports_parallel_tool_calls"] != true {
		t.Fatal("generated Codex model does not advertise parallel function calls")
	}
	if metadata["apply_patch_tool_type"] != "freeform" {
		t.Fatal("generated Codex model does not expose Codex's freeform apply_patch tool")
	}
	instructions, _ := metadata["base_instructions"].(string)
	if !strings.Contains(instructions, "SKILL.md") || !strings.Contains(instructions, "function tools") {
		t.Fatalf("generated model instructions omit skills or function tools: %q", instructions)
	}
}
