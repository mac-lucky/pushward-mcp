package main

import "testing"

func TestBuildAPITools_NoOrgOrPersonalSettingsTools(t *testing.T) {
	spec := apiSpec(t)
	inSpec := map[string]bool{}
	orgOps := 0
	for path, item := range spec.Paths {
		for _, op := range item {
			inSpec[op.OperationID] = true
			if isOrgPath(path) {
				orgOps++
			}
		}
	}
	// Otherwise the checks below pass on a spec that never had these operations.
	if orgOps == 0 {
		t.Fatal("openapi.yaml has no /orgs operations")
	}
	personal := map[string]bool{}
	for id := range personalSettingsOps {
		if !inSpec[id] {
			t.Errorf("%s is not in openapi.yaml: drop it from personalSettingsOps", id)
		}
		personal[toSnakeCase(id)] = true
	}

	for _, tool := range buildAPITools(spec) {
		if isOrgPath(tool.Path) {
			t.Errorf("%s is generated from %s", tool.Name, tool.Path)
		}
		if personal[tool.Name] {
			t.Errorf("%s is generated from a personal settings operation", tool.Name)
		}
	}
}

// The org rule matches /orgs and its subpaths only, and covers query
// parameters too, so a paged org list never fails generation.
func TestSkipOperation_OrgPaths(t *testing.T) {
	paged := []parameter{{Name: "cursor", In: "query"}}
	spec := &openAPISpec{Paths: map[string]pathItem{
		"/orgs":            {"get": operation{OperationID: "listOrgs", Parameters: paged}},
		"/orgs/{id}/items": {"get": operation{OperationID: "listOrgItems", Parameters: paged}},
		"/orgsettings":     {"get": operation{OperationID: "getOrgSettings"}},
	}}
	if got := unhandledQueryParams(spec); len(got) != 0 {
		t.Errorf("unhandledQueryParams = %v, want none", got)
	}
	var names []string
	for _, tool := range buildAPITools(spec) {
		names = append(names, tool.Name)
	}
	if len(names) != 1 || names[0] != "get_org_settings" {
		t.Errorf("tools = %v, want [get_org_settings]", names)
	}
}
