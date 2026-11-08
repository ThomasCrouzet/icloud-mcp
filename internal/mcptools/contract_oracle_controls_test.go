package mcptools

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func contractHasPath(args map[string]any, path []string) bool {
	var current any = args
	for _, key := range path {
		if key == "*" {
			array, ok := current.([]any)
			if !ok || len(array) == 0 {
				return false
			}
			current = array[0]
		} else {
			object, ok := current.(map[string]any)
			if !ok {
				return false
			}
			var exists bool
			current, exists = object[key]
			if !exists {
				return false
			}
		}
	}
	return true
}

func contractCoverageError(t *testing.T, tool mcp.Tool, specs map[string]contractBusinessFixture) error {
	t.Helper()
	spec, ok := specs[tool.Name]
	if !ok {
		return fmt.Errorf("registered tool %s lacks business fixture", tool.Name)
	}
	schema := contractInputSchema(t, tool)
	values := contractMerge(spec.base, spec.values)
	for _, p := range contractProperties(schema, nil) {
		if !contractHasPath(values, p.path) {
			return fmt.Errorf("published property %s.%s lacks explicit business value", tool.Name, strings.Join(p.path, "/"))
		}
	}
	props, _ := schema["properties"].(map[string]any)
	for key := range values {
		if _, ok := props[key]; !ok {
			return fmt.Errorf("stale fixture property %s.%s", tool.Name, key)
		}
	}
	return nil
}

func TestContractCompletenessOracleControls(t *testing.T) {
	f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
	for _, tool := range f.tools {
		if err := contractCoverageError(t, tool, contractBusinessFixtures()); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"tool", "property", "nested-property"} {
		t.Run(kind, func(t *testing.T) {
			tool := f.tools["create_contact"]
			if kind == "tool" {
				tool.Name = "new_contract_tool"
			} else {
				schema := contractInputSchema(t, tool)
				props := schema["properties"].(map[string]any)
				if kind == "nested-property" {
					props = props["name"].(map[string]any)["properties"].(map[string]any)
				}
				props["new_contract_property"] = map[string]any{"type": "string"}
				raw, err := json.Marshal(schema)
				if err != nil {
					t.Fatal(err)
				}
				tool.RawInputSchema = raw
			}
			if err := contractCoverageError(t, tool, contractBusinessFixtures()); err == nil {
				t.Fatalf("coverage oracle silently accepted uncovered %s", kind)
			} else {
				t.Logf("contract_completeness_control=%s error=%v", kind, err)
			}
		})
	}
}

func TestContractOutputOracleEveryDeclaredTool(t *testing.T) {
	seed := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
	for name, tool := range seed.tools {
		output, declared := contractWireTool(t, tool)["outputSchema"]
		if !declared {
			continue
		}
		t.Run(name, func(t *testing.T) {
			f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
			args := f.arguments(name, "")
			result := f.call(t, name, args)
			if result.IsError {
				t.Fatal(resultText(t, result))
			}
			contractValidateOutput(t, tool, result)
			schema := output.(map[string]any)
			props := schema["properties"].(map[string]any)
			compiled := contractCompileSchema(t, schema)
			for key, value := range props {
				t.Run(key, func(t *testing.T) {
					property := value.(map[string]any)
					mutated := contractPayload(t, result)
					typeValue := property["type"]
					var wrong any = "wrong-output-type"
					if typeValue == "string" {
						wrong = 123
					}
					if types, ok := typeValue.([]any); ok {
						for _, kind := range types {
							if kind == "string" {
								wrong = 123
							}
						}
					}
					if typeValue == "object" {
						wrong = []any{}
					}
					mutated[key] = wrong
					if err := compiled.Validate(contractValue(mutated)); err == nil {
						t.Fatalf("output oracle accepted wrong %s field type: %v", key, property)
					}
					t.Logf("contract_output_control=%s", contractReplayJSON(t, map[string]any{"tool": name, "property": key, "result": "rejected-mutated-actual-output"}))
				})
			}
			required, _ := schema["required"].([]any)
			for _, field := range required {
				mutated := contractPayload(t, result)
				delete(mutated, field.(string))
				if err := compiled.Validate(contractValue(mutated)); err == nil {
					t.Fatalf("output oracle accepted missing required field %s", field)
				}
			}
		})
	}
}
