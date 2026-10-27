package main

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

func TestDocumentedStoreSourceExistsAndListsPlugin(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	source := regexp.MustCompile(`https://raw\.githubusercontent\.com/brooke1220/keeper-usage-push/main/([^"\s]+)`)
	matches := source.FindAllSubmatch(readme, -1)
	if len(matches) == 0 {
		t.Fatal("README has no plugin store source")
	}
	for _, match := range matches {
		path := string(match[1])
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("documented store source %s is unavailable: %v", path, err)
		}
		var registry struct {
			SchemaVersion int `json:"schema_version"`
			Plugins       []struct {
				ID         string `json:"id"`
				Repository string `json:"repository"`
			} `json:"plugins"`
		}
		if err := json.Unmarshal(raw, &registry); err != nil {
			t.Fatalf("invalid store registry %s: %v", path, err)
		}
		if registry.SchemaVersion != 1 {
			t.Fatalf("store registry %s uses an unsupported schema", path)
		}
		found := false
		for _, plugin := range registry.Plugins {
			if plugin.ID == "keeper-usage-push" && plugin.Repository == "https://github.com/brooke1220/keeper-usage-push" {
				found = true
			}
		}
		if !found {
			t.Fatalf("store registry %s does not identify this plugin", path)
		}
	}
}
