package mcptools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type contractProperty struct {
	path     []string
	schema   map[string]any
	required bool
}

func contractProperties(schema map[string]any, prefix []string) []contractProperty {
	props, _ := schema["properties"].(map[string]any)
	required := map[string]bool{}
	fields, _ := schema["required"].([]any)
	for _, key := range fields {
		required[key.(string)] = true
	}
	var out []contractProperty
	keys := make([]string, 0, len(props))
	for key := range props {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		p := props[key].(map[string]any)
		path := append(append([]string(nil), prefix...), key)
		out = append(out, contractProperty{path, p, required[key]})
		if p["type"] == "object" {
			out = append(out, contractProperties(p, path)...)
		}
		if items, ok := p["items"].(map[string]any); ok && items["type"] == "object" {
			out = append(out, contractProperties(items, append(path, "*"))...)
		}
	}
	return out
}

func contractPathSet(args map[string]any, path []string, value any, remove bool) {
	var current any = args
	for _, key := range path[:len(path)-1] {
		if key == "*" {
			current = current.([]any)[0]
		} else {
			current = current.(map[string]any)[key]
		}
	}
	obj := current.(map[string]any)
	last := path[len(path)-1]
	if remove {
		delete(obj, last)
	} else {
		obj[last] = value
	}
}

func contractValue(value any) any {
	data, _ := json.Marshal(value)
	decoded, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		panic(err)
	}
	return decoded
}

func contractWrongTypes(kind string) []any {
	values := []any{nil, true, float64(7), "wrong-primitive", []any{}, map[string]any{}}
	var out []any
	for i, v := range values {
		if i == 1 && kind == "boolean" || i == 2 && (kind == "integer" || kind == "number") || i == 3 && kind == "string" || i == 4 && kind == "array" || i == 5 && kind == "object" {
			continue
		}
		out = append(out, v)
	}
	if kind == "integer" {
		out = append(out, 2.5)
	}
	return out
}

func contractInvalidBounds(schema map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems"} {
		raw, ok := schema[key].(json.Number)
		if !ok {
			continue
		}
		n, _ := raw.Float64()
		switch key {
		case "minimum":
			out["below-minimum"] = n - 1
		case "maximum":
			integer, ok := new(big.Int).SetString(raw.String(), 10)
			if !ok {
				panic("non-integer maximum in contract fixture")
			}
			integer.Add(integer, big.NewInt(1))
			out["above-maximum"] = json.Number(integer.String())
		case "minLength":
			if n > 0 {
				out["below-minLength"] = strings.Repeat("x", int(n)-1)
			}
		case "maxLength":
			out["above-maxLength"] = strings.Repeat("x", int(n)+1)
		case "minItems":
			if n > 0 {
				out["below-minItems"] = make([]any, int(n)-1)
			}
		case "maxItems":
			out["above-maxItems"] = make([]any, int(n)+1)
		}
	}
	if _, ok := schema["enum"]; ok {
		out["illegal-enum"] = "not-a-published-enum"
	}
	if _, ok := schema["pattern"]; ok {
		out["illegal-pattern"] = "! invalid \x00"
	}
	return out
}

func contractInputSchema(t *testing.T, tool mcp.Tool) map[string]any {
	t.Helper()
	return contractWireTool(t, tool)["inputSchema"].(map[string]any)
}

func contractArgumentCases(t *testing.T, tool mcp.Tool) {
	t.Helper()
	if err := contractCoverageError(t, tool, contractBusinessFixtures()); err != nil {
		t.Fatal(err)
	}
	schema := contractInputSchema(t, tool)
	for _, property := range contractProperties(schema, nil) {
		path := strings.Join(property.path, "/")
		t.Run(path, func(t *testing.T) {
			f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
			args := contractClone(f.arguments(tool.Name, property.path[0]))
			if err := contractCompileSchema(t, schema).Validate(contractValue(args)); err != nil {
				t.Fatalf("business fixture is not schema valid: %v", err)
			}
			for index, value := range contractWrongTypes(property.schema["type"].(string)) {
				t.Run(fmt.Sprintf("wrong-type-%d", index), func(t *testing.T) {
					bad := contractClone(args)
					contractPathSet(bad, property.path, value, false)
					f.refuseSDK(t, tool.Name, bad)
				})
			}
			bounds := contractInvalidBounds(property.schema)
			keys := make([]string, 0, len(bounds))
			for key := range bounds {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, label := range keys {
				t.Run(label, func(t *testing.T) {
					bad := contractClone(args)
					contractPathSet(bad, property.path, bounds[label], false)
					f.refuseSDK(t, tool.Name, bad)
				})
			}
			if property.schema["type"] == "object" {
				t.Run("unknown-nested", func(t *testing.T) {
					bad := contractClone(args)
					contractPathSet(bad, append(property.path, "undeclared_contract_key"), true, false)
					f.refuseSDK(t, tool.Name, bad)
				})
			}
			t.Run("absent", func(t *testing.T) {
				fresh := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
				absent := contractClone(fresh.arguments(tool.Name, property.path[0]))
				contractPathSet(absent, property.path, nil, true)
				if property.required {
					fresh.refuseSDK(t, tool.Name, absent)
					return
				}
				if len(property.path) == 1 {
					contractRepairAbsent(tool.Name, property.path[0], absent)
				}
				res := fresh.call(t, tool.Name, absent)
				if res.IsError {
					t.Fatalf("optional absence rejected: %s", resultText(t, res))
				}
				contractValidateOutput(t, tool, res)
				fresh.verify(t, tool.Name, absent, res)
			})
			t.Run("present-valid", func(t *testing.T) {
				res := f.call(t, tool.Name, args)
				if res.IsError {
					t.Fatalf("published valid property rejected: %s", resultText(t, res))
				}
				contractValidateOutput(t, tool, res)
				f.verify(t, tool.Name, args, res)
				f.verifyProperty(t, tool.Name, property.path, args, res)
			})
			if enums, ok := property.schema["enum"].([]any); ok {
				for index, value := range enums {
					t.Run(fmt.Sprintf("enum-%d", index), func(t *testing.T) {
						fresh := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
						a := contractClone(fresh.arguments(tool.Name, property.path[0]))
						contractPathSet(a, property.path, value, false)
						if property.path[0] == "scope" && value == "series" {
							delete(a, "recurrence_id")
						}
						res := fresh.call(t, tool.Name, a)
						if res.IsError {
							t.Fatalf("enum rejected: %s", resultText(t, res))
						}
						contractValidateOutput(t, tool, res)
						fresh.verify(t, tool.Name, a, res)
						fresh.verifyProperty(t, tool.Name, property.path, a, res)
					})
				}
			}
		})
	}
	t.Run("unknown-root", func(t *testing.T) {
		f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
		args := f.arguments(tool.Name, "")
		args["undeclared_contract_key"] = true
		f.refuseSDK(t, tool.Name, args)
	})
}

func contractRepairAbsent(tool, property string, args map[string]any) {
	if property == "all_day" {
		base := contractBusinessFixtures()[tool].base
		args["start"], args["end"] = base["start"], base["end"]
	}
	if property == "scope" || property == "recurrence_id" {
		delete(args, "scope")
		delete(args, "recurrence_id")
	}
	if tool == "create_contact" && property == "display_name" {
		args["name"] = map[string]any{"givenName": "Synthetic"}
	}
	if tool == "update_contact" && property == "display_name" {
		args["notes"] = "Changed"
	}
	if tool == "update_event" && property == "title" {
		args["location"] = "Changed"
	}
	if tool == "send_message" && property == "to" {
		args["cc"] = []any{"recipient@example.invalid"}
	}
}

func (f *contractRuntime) refuseSDK(t *testing.T, name string, args map[string]any) {
	t.Helper()
	before := f.counts()
	res := f.call(t, name, args)
	if !res.IsError || !strings.Contains(resultText(t, res), "input schema validation failed") {
		t.Fatalf("expected SDK refusal, not domain/business failure: %s", resultText(t, res))
	}
	if !reflect.DeepEqual(before, f.counts()) {
		t.Fatalf("invalid input reached a backend: before=%v after=%v", before, f.counts())
	}
}

func TestContractPublishedArguments(t *testing.T) {
	c, _ := contractManifestClient(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
	listed := contractListExact(t, c)
	for _, tool := range listed.Tools {
		t.Run(tool.Name, func(t *testing.T) { contractArgumentCases(t, tool) })
	}
}

func (f *contractRuntime) verifyProperty(t *testing.T, name string, path []string, args map[string]any, res *mcp.CallToolResult) {
	t.Helper()
	f.verifyValues(t, name, path, args, res)
}
