package mcptools

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestContractBooleanAndArrayItems(t *testing.T) {
	seed := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
	names := make([]string, 0, len(seed.tools))
	for name := range seed.tools {
		names = append(names, name)
	}
	sortContractNames(names)
	for _, name := range names {
		tool := seed.tools[name]
		t.Run(name, func(t *testing.T) {
			for _, p := range contractProperties(contractInputSchema(t, tool), nil) {
				if len(p.path) != 1 {
					continue
				}
				key := p.path[0]
				if p.schema["type"] == "boolean" {
					for _, value := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/bool-%v", key, value), func(t *testing.T) {
							f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
							args := f.arguments(name, key)
							args[key] = value
							if key == "all_day" && !value {
								base := contractBusinessFixtures()[name].base
								args["start"], args["end"] = base["start"], base["end"]
							}
							res := f.call(t, name, args)
							if res.IsError {
								t.Fatalf("published boolean rejected: %s", resultText(t, res))
							}
							contractValidateOutput(t, tool, res)
							f.verify(t, name, args, res)
							f.verifyProperty(t, name, p.path, args, res)
						})
					}
					continue
				}
				if p.schema["type"] != "array" {
					continue
				}
				t.Run(key+"/items", func(t *testing.T) {
					f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
					args := contractClone(f.arguments(name, key))
					items := p.schema["items"].(map[string]any)
					for i, value := range contractWrongTypes(items["type"].(string)) {
						t.Run(fmt.Sprintf("wrong-item-type-%d", i), func(t *testing.T) { bad := contractClone(args); bad[key] = []any{value}; f.refuseSDK(t, name, bad) })
					}
					if items["type"] == "object" {
						bad := contractClone(args)
						bad[key].([]any)[0].(map[string]any)["undeclared_contract_key"] = true
						f.refuseSDK(t, name, bad)
					}
					for label, value := range contractInvalidBounds(items) {
						t.Run("item-"+label, func(t *testing.T) { bad := contractClone(args); bad[key] = []any{value}; f.refuseSDK(t, name, bad) })
					}
					if p.schema["uniqueItems"] == true {
						bad := contractClone(args)
						first := bad[key].([]any)[0]
						bad[key] = []any{first, first}
						f.refuseSDK(t, name, bad)
					}
					if enums, ok := items["enum"].([]any); ok {
						for i, value := range enums {
							t.Run(fmt.Sprintf("item-enum-%d", i), func(t *testing.T) {
								a := contractClone(seed.arguments(name, key))
								a[key] = []any{value}
								fresh := contractBoundRuntime(t, a)
								res := fresh.call(t, name, a)
								if res.IsError {
									t.Fatalf("item enum rejected: %s", resultText(t, res))
								}
								contractValidateOutput(t, tool, res)
								fresh.verify(t, name, a, res)
								fresh.verifyProperty(t, name, p.path, a, res)
							})
						}
					}
					if items["type"] == "string" {
						for _, bound := range []string{"minLength", "maxLength"} {
							raw, ok := items[bound].(json.Number)
							if !ok {
								continue
							}
							t.Run("item-exact-"+bound, func(t *testing.T) {
								a := contractClone(seed.arguments(name, key))
								value := contractBoundValue(contractProperty{path: []string{key, "*"}, schema: items}, bound, raw, a[key].([]any)[0])
								a[key] = []any{value}
								fresh := contractBoundRuntime(t, a)
								res := fresh.call(t, name, a)
								if res.IsError {
									t.Fatalf("exact item bound rejected: %s", resultText(t, res))
								}
								contractValidateOutput(t, tool, res)
								fresh.verifyProperty(t, name, p.path, a, res)
							})
						}
					}
				})
			}
		})
	}
}
