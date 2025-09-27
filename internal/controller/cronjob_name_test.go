package controller

import (
	"fmt"
	"strings"
	"testing"
)

func TestGenerateShortCronJobName(t *testing.T) {
	tests := []struct {
		name      string
		groupName string
		nodeName  string
		maxLength int
	}{
		{
			name:      "short names",
			groupName: "test-group",
			nodeName:  "test-node",
			maxLength: 52,
		},
		{
			name:      "long names that need truncation",
			groupName: "test-group-conditions-1758959759849012000",
			nodeName:  "test-k8s-node-conditions-1758959759849013000",
			maxLength: 52,
		},
		{
			name:      "very long names",
			groupName: "extremely-long-group-name-that-exceeds-normal-limits",
			nodeName:  "extremely-long-kubernetes-node-name-that-exceeds-normal-limits",
			maxLength: 52,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := generateShortCronJobName(tt.groupName, tt.nodeName)

			// Check that the name doesn't exceed the Kubernetes limit
			if len(result) > tt.maxLength {
				t.Errorf("generateShortCronJobName() = %v, length = %v, want max length = %v", result, len(result), tt.maxLength)
			}

			// Check that the name ends with -healthcheck
			if len(result) < 12 || result[len(result)-12:] != "-healthcheck" {
				t.Errorf("generateShortCronJobName() = %v, should end with -healthcheck", result)
			}

			// Check that the name starts with the expected prefix (groupName-nodeName)
			expectedPrefix := fmt.Sprintf("%s-%s", tt.groupName, tt.nodeName)
			if !strings.HasPrefix(result, expectedPrefix) {
				// For truncated names, check if it starts with a truncated version
				if len(tt.groupName)+len(tt.nodeName) > 29 { // maxNameLength from generateShortCronJobName
					// Should start with at least part of the group name
					if !strings.HasPrefix(result, tt.groupName[:1]) {
						t.Errorf("generateShortCronJobName() = %v, should start with group name or its truncated version", result)
					}
				} else {
					t.Errorf("generateShortCronJobName() = %v, should start with %s", result, expectedPrefix)
				}
			}

			t.Logf("Generated name: %s (length: %d)", result, len(result))
		})
	}
}

func TestExtractNodeNameFromCronJob(t *testing.T) {
	tests := []struct {
		name          string
		cronJobName   string
		cronJobLabels map[string]string
		expectedNode  string
	}{
		{
			name:          "new format with labels",
			cronJobName:   "test-group-test-node-a1b2c3d4-healthcheck",
			cronJobLabels: map[string]string{"node": "test-node"},
			expectedNode:  "test-node",
		},
		{
			name:          "new format without labels (fallback)",
			cronJobName:   "test-group-test-node-a1b2c3d4-healthcheck",
			cronJobLabels: map[string]string{},
			expectedNode:  "test-node", // Should extract from name
		},
		{
			name:          "old format (backward compatibility)",
			cronJobName:   "test-group-test-node-healthcheck",
			cronJobLabels: map[string]string{},
			expectedNode:  "test-node",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractNodeNameFromCronJob(tt.cronJobName, tt.cronJobLabels)
			if result != tt.expectedNode {
				t.Errorf("extractNodeNameFromCronJob() = %v, want %v", result, tt.expectedNode)
			}
		})
	}
}
