#!/bin/bash

# Install homelab-autoscaler with cluster autoscaler integration
# This script demonstrates how to install the homelab-autoscaler Helm chart
# with cluster autoscaler enabled for automatic node scaling.

set -e

# Configuration
NAMESPACE="homelab-autoscaler-system"
RELEASE_NAME="homelab-autoscaler"
CHART_PATH="dist/chart"
VALUES_FILE="examples/cluster-autoscaler-values.yaml"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Function to print colored output
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

# Function to check if command exists
command_exists() {
    command -v "$1" >/dev/null 2>&1
}

# Check prerequisites
check_prerequisites() {
    print_status "Checking prerequisites..."
    
    if ! command_exists helm; then
        print_error "Helm is not installed. Please install Helm first."
        exit 1
    fi
    
    if ! command_exists kubectl; then
        print_error "kubectl is not installed. Please install kubectl first."
        exit 1
    fi
    
    # Check if kubectl can connect to cluster
    if ! kubectl cluster-info >/dev/null 2>&1; then
        print_error "Cannot connect to Kubernetes cluster. Please check your kubeconfig."
        exit 1
    fi
    
    print_success "Prerequisites check passed"
}

# Create namespace if it doesn't exist
create_namespace() {
    print_status "Creating namespace '$NAMESPACE' if it doesn't exist..."
    
    if kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
        print_warning "Namespace '$NAMESPACE' already exists"
    else
        kubectl create namespace "$NAMESPACE"
        print_success "Namespace '$NAMESPACE' created"
    fi
}

# Install cert-manager if not present
install_cert_manager() {
    print_status "Checking if cert-manager is installed..."
    
    if kubectl get namespace cert-manager >/dev/null 2>&1; then
        print_warning "cert-manager namespace already exists, assuming cert-manager is installed"
    else
        print_status "Installing cert-manager..."
        kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.13.0/cert-manager.yaml
        
        print_status "Waiting for cert-manager to be ready..."
        kubectl wait --for=condition=Available --timeout=300s deployment/cert-manager -n cert-manager
        kubectl wait --for=condition=Available --timeout=300s deployment/cert-manager-cainjector -n cert-manager
        kubectl wait --for=condition=Available --timeout=300s deployment/cert-manager-webhook -n cert-manager
        
        print_success "cert-manager installed successfully"
    fi
}

# Validate chart and values
validate_chart() {
    print_status "Validating Helm chart..."
    
    if [ ! -d "$CHART_PATH" ]; then
        print_error "Chart directory '$CHART_PATH' not found. Please run 'make helm-package' first."
        exit 1
    fi
    
    if [ ! -f "$VALUES_FILE" ]; then
        print_error "Values file '$VALUES_FILE' not found."
        exit 1
    fi
    
    # Lint the chart
    if helm lint "$CHART_PATH" -f "$VALUES_FILE"; then
        print_success "Chart validation passed"
    else
        print_error "Chart validation failed"
        exit 1
    fi
}

# Install or upgrade the chart
install_chart() {
    print_status "Installing homelab-autoscaler with cluster autoscaler..."
    
    helm upgrade --install "$RELEASE_NAME" "$CHART_PATH" \
        --namespace "$NAMESPACE" \
        --values "$VALUES_FILE" \
        --wait \
        --timeout=10m
    
    print_success "homelab-autoscaler installed successfully"
}

# Verify installation
verify_installation() {
    print_status "Verifying installation..."
    
    # Check if all pods are running
    print_status "Checking pod status..."
    kubectl get pods -n "$NAMESPACE"
    
    # Wait for deployments to be ready
    print_status "Waiting for deployments to be ready..."
    kubectl wait --for=condition=Available --timeout=300s deployment/homelab-autoscaler-controller-manager -n "$NAMESPACE"
    kubectl wait --for=condition=Available --timeout=300s deployment/homelab-autoscaler-cluster-autoscaler -n "$NAMESPACE"
    
    # Check services
    print_status "Checking services..."
    kubectl get services -n "$NAMESPACE"
    
    # Check if gRPC service is accessible
    print_status "Checking gRPC service..."
    if kubectl get service homelab-autoscaler-grpc-service -n "$NAMESPACE" >/dev/null 2>&1; then
        print_success "gRPC service is available"
    else
        print_error "gRPC service not found"
        exit 1
    fi
    
    print_success "Installation verification completed"
}

# Display usage information
show_usage() {
    print_status "Installation completed! Here's how to use the cluster autoscaler:"
    echo ""
    echo "1. Create node groups using the homelab-autoscaler CRDs:"
    echo "   kubectl apply -f examples/k3d/group1.yaml -n $NAMESPACE"
    echo ""
    echo "2. Monitor cluster autoscaler logs:"
    echo "   kubectl logs -f deployment/homelab-autoscaler-cluster-autoscaler -n $NAMESPACE"
    echo ""
    echo "3. Monitor controller manager logs:"
    echo "   kubectl logs -f deployment/homelab-autoscaler-controller-manager -n $NAMESPACE"
    echo ""
    echo "4. Check cluster autoscaler status:"
    echo "   kubectl describe configmap cluster-autoscaler-status -n kube-system"
    echo ""
    echo "5. Scale test (create a deployment that requires more nodes):"
    echo "   kubectl create deployment test-scale --image=nginx --replicas=20"
    echo ""
    echo "6. Uninstall (if needed):"
    echo "   helm uninstall $RELEASE_NAME -n $NAMESPACE"
    echo ""
}

# Main execution
main() {
    print_status "Starting homelab-autoscaler installation with cluster autoscaler..."
    
    check_prerequisites
    create_namespace
    install_cert_manager
    validate_chart
    install_chart
    verify_installation
    show_usage
    
    print_success "Installation completed successfully!"
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        -n|--namespace)
            NAMESPACE="$2"
            shift 2
            ;;
        -r|--release)
            RELEASE_NAME="$2"
            shift 2
            ;;
        -f|--values)
            VALUES_FILE="$2"
            shift 2
            ;;
        -h|--help)
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  -n, --namespace NAMESPACE    Kubernetes namespace (default: homelab-autoscaler-system)"
            echo "  -r, --release RELEASE        Helm release name (default: homelab-autoscaler)"
            echo "  -f, --values VALUES_FILE     Values file path (default: examples/cluster-autoscaler-values.yaml)"
            echo "  -h, --help                   Show this help message"
            echo ""
            echo "Example:"
            echo "  $0 --namespace my-namespace --values my-values.yaml"
            exit 0
            ;;
        *)
            print_error "Unknown option: $1"
            echo "Use --help for usage information"
            exit 1
            ;;
    esac
done

# Run main function
main