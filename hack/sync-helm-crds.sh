#!/bin/bash

# sync-helm-crds.sh - Synchronize kubebuilder-generated CRDs to Helm chart templates
# This script transforms CRDs from config/crd/bases/ into Helm-templated versions
# in dist/chart/templates/crd/ while preserving Helm functionality.

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

# Function to transform a CRD file into Helm template format
transform_crd() {
    local source_file="$1"
    local target_file="$2"
    local crd_name="$(basename "$source_file")"

    print_status "Transforming $crd_name..."

    # Create target directory if it doesn't exist
    mkdir -p "$(dirname "$target_file")"

    # Check if source file exists
    if [ ! -f "$source_file" ]; then
        print_error "Source file $source_file does not exist"
        return 1
    fi

    # Create the Helm-templated CRD
    {
        echo "{{- if .Values.crd.enable }}"
        echo "---"

        # Process the source file line by line
        local in_metadata=false
        local metadata_labels_added=false

        while IFS= read -r line; do
            # Skip the initial document separator
            if [[ "$line" == "---" ]]; then
                continue
            fi

            # Handle metadata section
            if [[ "$line" == "metadata:" ]]; then
                echo "$line"
                in_metadata=true
                metadata_labels_added=false
                continue
            fi

            # If we're in metadata and haven't added Helm labels yet
            if [[ "$in_metadata" == true ]] && [[ "$metadata_labels_added" == false ]]; then
                # Check if this is annotations or labels that we want to replace
                if [[ "$line" =~ ^[[:space:]]*annotations: ]] || [[ "$line" =~ ^[[:space:]]*labels: ]]; then
                    # Add Helm labels and annotations first
                    echo "  labels:"
                    echo "    {{- include \"chart.labels\" . | nindent 4 }}"
                    echo "    {{- if .Values.crd.labels }}"
                    echo "    {{- toYaml .Values.crd.labels | nindent 4 }}"
                    echo "    {{- end }}"
                    echo "  annotations:"
                    echo "    {{- if .Values.crd.keep }}"
                    echo "    \"helm.sh/resource-policy\": keep"
                    echo "    {{- end }}"
                    echo "    {{- if .Values.crd.annotations }}"
                    echo "    {{- toYaml .Values.crd.annotations | nindent 4 }}"
                    echo "    {{- end }}"
                    metadata_labels_added=true

                    # Skip the original annotations/labels section
                    while IFS= read -r skip_line; do
                        # Check if this line starts a new top-level field (not indented more than current)
                        if [[ "$skip_line" =~ ^[[:space:]]*[a-zA-Z] ]] && [[ ! "$skip_line" =~ ^[[:space:]]{3,} ]]; then
                            # This is the next metadata field, don't consume it - put it back
                            # We need to process this line in the main loop
                            echo "$skip_line"
                            break
                        elif [[ "$skip_line" =~ ^[^[:space:]] ]]; then
                            # End of metadata section entirely
                            echo "$skip_line"
                            in_metadata=false
                            break
                        fi
                        # Otherwise, this line is part of the annotations/labels we're skipping
                    done
                    continue
                else
                    # This is a regular metadata field (like name), add Helm labels first if needed
                    if [[ "$metadata_labels_added" == false ]]; then
                        echo "  labels:"
                        echo "    {{- include \"chart.labels\" . | nindent 4 }}"
                        echo "    {{- if .Values.crd.labels }}"
                        echo "    {{- toYaml .Values.crd.labels | nindent 4 }}"
                        echo "    {{- end }}"
                        echo "  annotations:"
                        echo "    {{- if .Values.crd.keep }}"
                        echo "    \"helm.sh/resource-policy\": keep"
                        echo "    {{- end }}"
                        echo "    {{- if .Values.crd.annotations }}"
                        echo "    {{- toYaml .Values.crd.annotations | nindent 4 }}"
                        echo "    {{- end }}"
                        metadata_labels_added=true
                    fi
                    echo "$line"
                    continue
                fi
            fi

            # Check if we're leaving metadata section
            if [[ "$in_metadata" == true ]] && [[ "$line" =~ ^[^[:space:]] ]]; then
                in_metadata=false
            fi

            echo "$line"

        done < "$source_file"

        echo "{{- end -}}"
    } > "$target_file"

    # Check if the file was created successfully
    if [ -f "$target_file" ]; then
        print_success "Generated $target_file"
        return 0
    else
        print_error "Failed to generate $target_file"
        return 1
    fi
}

# Function to read version from Chart.yaml
get_chart_version() {
    local chart_file="dist/chart/Chart.yaml"
    
    if [ ! -f "$chart_file" ]; then
        print_error "Chart.yaml not found at $chart_file"
        return 1
    fi
    
    # Extract version using grep and sed
    local version=$(grep "^version:" "$chart_file" | sed 's/version:[[:space:]]*//')
    
    if [ -z "$version" ]; then
        print_error "Could not extract version from $chart_file"
        return 1
    fi
    
    echo "$version"
}

# Function to update image configuration in values.yaml
update_image_config() {
    local values_file="dist/chart/values.yaml"
    local chart_version="$1"

    if [ ! -f "$values_file" ]; then
        print_error "values.yaml not found at $values_file"
        return 1
    fi

    print_status "Updating image configuration in $values_file..."

    # Backup the original file
    cp "$values_file" "${values_file}.bak"

    # Extract the current repository and tag values from the controllerManager section
    local current_repo=$(awk '/controllerManager:/,/^[[:space:]]*[a-zA-Z]/ {if ($1 == "repository:") {getline; print $2; exit}}' "$values_file" | sed 's/^[[:space:]]*"//;s/"[[:space:]]*$//' | xargs)
    local current_tag=$(awk '/controllerManager:/,/^[[:space:]]*[a-zA-Z]/ {if ($1 == "tag:") {getline; print $2; exit}}' "$values_file" | sed 's/^[[:space:]]*"//;s/"[[:space:]]*$//' | xargs)

    # If we couldn't extract the values, use defaults
    if [ -z "$current_repo" ]; then
        current_repo="homeclusterdev/autoscaler"
    fi
    if [ -z "$current_tag" ]; then
        current_tag="0.1.12"
    fi

    # Update the controllerManager repository line to use templating with default
    awk -v repo="$current_repo" '
    /controllerManager:/,/^[[:space:]]*[a-zA-Z]/ {
        if ($1 == "repository:") {
            print $1 " {{ .Values.controllerManager.container.image.repository | default \"" repo "\" }}"
            getline
            next
        }
    }
    { print }
    ' "$values_file" > "${values_file}.tmp" && mv "${values_file}.tmp" "$values_file"

    # Update the controllerManager tag line to use templating with default
    awk -v tag="$current_tag" '
    /controllerManager:/,/^[[:space:]]*[a-zA-Z]/ {
        if ($1 == "tag:") {
            print $1 " {{ .Values.controllerManager.container.image.tag | default \"" tag "\" }}"
            getline
            next
        }
    }
    { print }
    ' "$values_file" > "${values_file}.tmp" && mv "${values_file}.tmp" "$values_file"

    # Verify the changes were made
    if grep -q "repository: {{ .Values.controllerManager.container.image.repository | default " "$values_file" && \
       grep -q "tag: {{ .Values.controllerManager.container.image.tag | default " "$values_file"; then
        print_success "Successfully updated image configuration with templating and defaults"
        # Remove backup file
        rm -f "${values_file}.bak"
        return 0
    else
        print_error "Failed to update image configuration"
        # Restore from backup if update failed
        if [ -f "${values_file}.bak" ]; then
            mv "${values_file}.bak" "$values_file"
        fi
        return 1
    fi
}

# Function to validate Helm templates
validate_helm_templates() {
    print_status "Validating Helm templates..."
    
    cd "$PROJECT_ROOT/dist/chart"
    
    if helm template . --dry-run > /dev/null; then
        print_success "Helm templates are valid"
        return 0
    else
        print_error "Helm template validation failed"
        return 1
    fi
}

# Main execution
main() {
    print_status "Starting CRD synchronization from kubebuilder to Helm chart..."
    
    # Change to project root
    cd "$PROJECT_ROOT"
    
    # Check if source directory exists
    if [ ! -d "$SOURCE_DIR" ]; then
        print_error "Source directory '$SOURCE_DIR' not found. Run 'make manifests' first."
        exit 1
    fi
    
    # Create target directory
    mkdir -p "$TARGET_DIR"
    
    # Remove outdated files first
    outdated_files=(
        "$TARGET_DIR/_groups.yaml"
    )
    
    for outdated_file in "${outdated_files[@]}"; do
        if [ -f "$outdated_file" ]; then
            print_status "Removing outdated file: $outdated_file"
            rm -f "$outdated_file"
        fi
    done
    
    # Find all CRD files in source directory
    if ! find "$SOURCE_DIR" -name "*.yaml" -type f | head -1 > /dev/null 2>&1; then
        print_warning "No CRD files found in '$SOURCE_DIR'"
        exit 0
    fi
    
    # Transform each CRD file
    find "$SOURCE_DIR" -name "*.yaml" -type f | while read -r source_file; do
        filename=$(basename "$source_file")
        target_file="$TARGET_DIR/$filename"
        
        transform_crd "$source_file" "$target_file"
    done
    
    print_success "CRD synchronization completed successfully!"
    print_status "Synchronized CRDs:"
    ls -la "$TARGET_DIR"/*.yaml 2>/dev/null || print_warning "No CRD files found in target directory"
    
    # Note: Image configuration is already properly templated in values.yaml
    print_status "Skipping image configuration update - already properly templated in values.yaml"
    
    # Validate Helm templates
    validate_helm_templates
    if [ $? -ne 0 ]; then
        exit 1
    fi
}

# Run main function
main "$@"