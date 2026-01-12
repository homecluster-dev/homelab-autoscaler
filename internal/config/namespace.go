package config

import (
	"fmt"
	"os"
	"strings"
)

// NamespaceConfig holds the namespace configuration
type NamespaceConfig struct {
	namespace string
}

// NewNamespaceConfig creates a new namespace configuration with hierarchical precedence
// 1. Environment variable: HOMELAB_AUTOSCALER_NAMESPACE
// 2. Default value: "homelab-autoscaler-system"
func NewNamespaceConfig() *NamespaceConfig {
	defaultNamespace := "homelab-autoscaler-system"

	// Check for environment variable first
	if envNamespace := os.Getenv("HOMELAB_AUTOSCALER_NAMESPACE"); envNamespace != "" {
		defaultNamespace = envNamespace
	}

	return &NamespaceConfig{
		namespace: defaultNamespace,
	}
}

// Get returns the configured namespace
func (n *NamespaceConfig) Get() string {
	return n.namespace
}

// Set allows setting the namespace programmatically (useful for testing)
func (n *NamespaceConfig) Set(namespace string) {
	n.namespace = namespace
}

// Validate validates the namespace name according to Kubernetes naming conventions
func (n *NamespaceConfig) Validate() error {
	ns := n.Get()

	// Kubernetes namespace naming rules:
	// - Must be lowercase
	// - Must contain only alphanumeric characters or hyphens
	// - Must start and end with an alphanumeric character
	// - Must be 63 characters or fewer
	if len(ns) == 0 || len(ns) > 63 {
		return fmt.Errorf("namespace must be 1-63 characters long")
	}

	// Ensure the namespace contains at least one alphanumeric character.
	// The previous condition also checked for a hyphen, which is unnecessary for this rule.
	if !strings.ContainsAny(ns, "abcdefghijklmnopqrstuvwxyz0123456789") {
		return fmt.Errorf("namespace must contain at least one alphanumeric character")
	}

	for i, c := range ns {
		// Reject characters that are not lower‑case letters, digits, or hyphens.
		// The original condition used a double negated expression which staticcheck flags.
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return fmt.Errorf("namespace contains invalid character: %q", c)
		}

		if i == 0 && (c == '-' || (c >= '0' && c <= '9')) {
			return fmt.Errorf("namespace must start with an alphabetic character")
		}

		if i == len(ns)-1 && c == '-' {
			return fmt.Errorf("namespace must end with an alphanumeric character")
		}
	}

	return nil
}

// GetOrDefault returns the namespace or a default if validation fails
func (n *NamespaceConfig) GetOrDefault() string {
	if err := n.Validate(); err != nil {
		return "homelab-autoscaler-system"
	}
	return n.Get()
}
