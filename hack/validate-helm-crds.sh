#!/bin/bash

# validate-helm-crds.sh - Validate that Helm chart CRDs match kubebuilder-generated CRDs
# This script compares the schema sections of CRDs to ensure they are in sync.

set -e

# Configuration
SOURCE_DIR="config/crd/bases"
TARGET_DIR="dist/chart/templates/crd"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

print_status() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

print_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Function to extract schema from CRD file
extract_schema() {
    local file="$1"
    local temp_file=$(mktemp)
    
    # Extract just the spec section (which contains the schema)
    awk '/^spec:$/,/^[a-zA-Z]/ {
        if (/^[a-zA-Z]/ && !/^spec:$/) exit
        print
    }' "$file" > "$temp_file"
    
    echo "$temp_file"
}

# Function to validate a single CRD
validate_crd() {
    local source_file="$1"
    local target_file="$2"
    local crd_name="$(basename "$source_file")"
    
    print_status "Validating $crd_name..."
    
    if [ ! -f "$target_file" ]; then
        print_error "Target file not found: $target_file"
        return 1
    fi
    
    # Extract schemas from both files
    local source_schema=$(extract_schema "$source_file")
    local target_schema=$(extract_schema "$target_file")
    
    # Compare schemas
    if diff -u "$source_schema" "$target_schema" > /dev/null; then
        print_success "$crd_name schemas match"
        rm -f "$source_schema" "$target_schema"
        return 0
    else
        print_error "$crd_name schemas differ:"
        echo "--- Source (kubebuilder)"
        echo "+++ Target (Helm chart)"
        diff -u "$source_schema" "$target_schema" || true
        rm -f "$source_schema" "$target_schema"
        return 1
    fi
}

# Function to validate Helm templating
validate_helm_templating() {
    local target_file="$1"
    local crd_name="$(basename "$target_file")"
    
    print_status "Validating Helm templating in $crd_name..."
    
    local errors=0
    
    # Check for required Helm conditionals
    if ! grep -q "{{- if .Values.crd.enable }}" "$target_file"; then
        print_error "$crd_name missing Helm conditional: {{- if .Values.crd.enable }}"
        errors=$((errors + 1))
    fi
    
    if ! grep -q "{{- end -}}" "$target_file"; then
        print_error "$crd_name missing Helm conditional end: {{- end -}}"
        errors=$((errors + 1))
    fi
    
    # Check for Helm labels
    if ! grep -q "{{- include \"chart.labels\" . | nindent 4 }}" "$target_file"; then
        print_error "$crd_name missing Helm labels template"
        errors=$((errors + 1))
    fi
    
    # Check for conditional keep annotation
    if ! grep -q "{{- if .Values.crd.keep }}" "$target_file"; then
        print_error "$crd_name missing conditional keep annotation"
        errors=$((errors + 1))
    fi
    
    if [ $errors -eq 0 ]; then
        print_success "$crd_name Helm templating is correct"
        return 0
    else
        return 1
    fi
}

# Main execution
main() {
    print_status "Starting CRD validation..."
    
    # Change to project root
    cd "$PROJECT_ROOT"
    
    # Check if directories exist
    if [ ! -d "$SOURCE_DIR" ]; then
        print_error "Source directory '$SOURCE_DIR' not found. Run 'make manifests' first."
        exit 1
    fi
    
    if [ ! -d "$TARGET_DIR" ]; then
        print_error "Target directory '$TARGET_DIR' not found. Run 'make helm-sync-crds' first."
        exit 1
    fi
    
    local validation_errors=0
    
    # Find all CRD files in source directory
    if ! find "$SOURCE_DIR" -name "*.yaml" -type f | head -1 > /dev/null 2>&1; then
        print_warning "No CRD files found in '$SOURCE_DIR'"
        exit 0
    fi
    
    # Validate each CRD file
    find "$SOURCE_DIR" -name "*.yaml" -type f | while read -r source_file; do
        filename=$(basename "$source_file")
        target_file="$TARGET_DIR/$filename"
        
        # Validate schema consistency
        if ! validate_crd "$source_file" "$target_file"; then
            validation_errors=$((validation_errors + 1))
        fi
        
        # Validate Helm templating
        if ! validate_helm_templating "$target_file"; then
            validation_errors=$((validation_errors + 1))
        fi
    done
    
    if [ $validation_errors -eq 0 ]; then
        print_success "All CRD validations passed!"
        exit 0
    else
        print_error "CRD validation failed with $validation_errors errors"
        exit 1
    fi
}

# Run main function
main "$@"