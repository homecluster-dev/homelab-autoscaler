#!/bin/bash

# Development installation script for homelab-autoscaler
# This script sets up a local development environment where:
# - The controller manager runs locally (outside cluster)
# - The cluster autoscaler runs in-cluster
# - A local service forwarder enables communication between them

set -e

# Configuration
CLUSTER_NAME="homelab-autoscaler"
NAMESPACE="homelab-autoscaler-system"
RELEASE_NAME="homelab-autoscaler-dev"
CHART_PATH="dist/chart"
VALUES_FILE="development/development-values.yaml"
K3D_SCRIPT="examples/k3d/create-cluster.sh"
SERVICE_FORWARDER="development/local-service-forwarder-dev.yaml"

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
    
    local missing_tools=()
    
    if ! command_exists k3d; then
        missing_tools+=("k3d")
    fi
    
    if ! command_exists helm; then
        missing_tools+=("helm")
    fi
    
    if ! command_exists kubectl; then
        missing_tools+=("kubectl")
    fi
    
    if [ ${#missing_tools[@]} -ne 0 ]; then
        print_error "Missing required tools: ${missing_tools[*]}"
        echo "Please install the missing tools and try again."
        exit 1
    fi
    
    print_success "Prerequisites check passed"
}

# Create k3d cluster
create_cluster() {
    print_status "Creating k3d cluster..."
    
    # Check if cluster already exists
    if k3d cluster list | grep -q "$CLUSTER_NAME"; then
        print_warning "Cluster '$CLUSTER_NAME' already exists"
        print_status "Deleting existing cluster..."
        k3d cluster delete "$CLUSTER_NAME"
    fi
    
    # Create new cluster using the existing script
    if [ ! -f "$K3D_SCRIPT" ]; then
        print_error "K3d creation script not found: $K3D_SCRIPT"
        exit 1
    fi
    
    print_status "Running k3d cluster creation script..."
    bash "$K3D_SCRIPT"
    
    print_success "K3d cluster created successfully"
}

# Setup kubeconfig
setup_kubeconfig() {
    print_status "Setting up kubeconfig..."
    
    # Export kubeconfig for the session
    export KUBECONFIG=$(pwd)/kubeconfig
    
    # Verify connection
    if kubectl cluster-info >/dev/null 2>&1; then
        print_success "Kubeconfig configured successfully"
        kubectl cluster-info
    else
        print_error "Failed to connect to cluster"
        exit 1
    fi
}

# Create namespace
create_namespace() {
    print_status "Creating namespace '$NAMESPACE'..."
    
    if kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
        print_warning "Namespace '$NAMESPACE' already exists"
    else
        kubectl create namespace "$NAMESPACE"
        print_success "Namespace '$NAMESPACE' created"
    fi
}

# Apply local service forwarder (after Helm install to avoid conflicts)
apply_service_forwarder() {
    print_status "Applying development local service forwarder..."
    
    if [ ! -f "$SERVICE_FORWARDER" ]; then
        print_error "Service forwarder file not found: $SERVICE_FORWARDER"
        exit 1
    fi
    
    # Apply the local service forwarder which creates services with different names
    # to avoid conflicts with Helm-managed services
    kubectl apply -f "$SERVICE_FORWARDER"
    print_success "Development local service forwarder applied"
}

# Validate chart and values
validate_chart() {
    print_status "Validating Helm chart..."
    
    if [ ! -d "$CHART_PATH" ]; then
        print_error "Chart directory '$CHART_PATH' not found."
        print_error "Please run 'make helm-package' first to build the chart."
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

# Install cluster autoscaler only
install_cluster_autoscaler() {
    print_status "Installing cluster autoscaler with development configuration..."
    
    helm upgrade --install "$RELEASE_NAME" "$CHART_PATH" \
        --namespace "$NAMESPACE" \
        --values "$VALUES_FILE" \
        --wait \
        --timeout=10m
    
    print_success "Cluster autoscaler installed successfully"
}

# Verify installation
verify_installation() {
    print_status "Verifying installation..."
    
    # Check if cluster autoscaler pod is running
    print_status "Checking cluster autoscaler pod status..."
    kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/component=cluster-autoscaler
        
    # Check services
    print_status "Checking services..."
    kubectl get services -n "$NAMESPACE"
    
    # Verify gRPC service forwarder is in place
    if kubectl get service homelab-autoscaler-grpc-local -n "$NAMESPACE" >/dev/null 2>&1; then
        print_success "Development gRPC service forwarder is available"
    else
        print_error "Development gRPC service forwarder not found"
        exit 1
    fi
    
    print_success "Installation verification completed"
}

# Display usage information
show_usage() {
    print_success "Development environment setup completed!"
    echo ""
    echo "=== NEXT STEPS ==="
    echo ""
    echo "1. Build and run the local gRPC server:"
    echo "   make build"
    echo "   ./bin/manager --enable-grpc-server=true --grpc-bind-address=:50052"
    echo ""
    echo "2. In another terminal, create test node groups:"
    echo "   export KUBECONFIG=$(pwd)/kubeconfig"
    echo "   kubectl apply -f examples/k3d/group1.yaml -n $NAMESPACE"
    echo ""
    echo "3. Monitor cluster autoscaler logs:"
    echo "   kubectl logs -f deployment/$RELEASE_NAME-cluster-autoscaler -n $NAMESPACE"
    echo ""
    echo "4. Monitor local controller manager logs in the terminal where you started it"
    echo ""
    echo "5. Test scaling by creating a deployment that requires more nodes:"
    echo "   kubectl create deployment test-scale --image=nginx --replicas=20"
    echo ""
    echo "=== IMPORTANT NOTES ==="
    echo ""
    echo "• The controller manager runs LOCALLY on port 50052"
    echo "• The cluster autoscaler runs IN-CLUSTER and connects via service forwarder"
    echo "• The development service forwarder maps cluster port 50051 to local port 50052"
    echo "• Make sure to run the local gRPC server before testing autoscaling"
    echo ""
    echo "=== CLEANUP ==="
    echo ""
    echo "To clean up the development environment:"
    echo "   helm uninstall $RELEASE_NAME -n $NAMESPACE"
    echo "   k3d cluster delete $CLUSTER_NAME"
    echo ""
}

# Main execution
main() {
    print_status "Starting homelab-autoscaler development environment setup..."
    
    check_prerequisites
    create_cluster
    setup_kubeconfig
    create_namespace
    validate_chart
    install_cluster_autoscaler
    apply_service_forwarder
    verify_installation
    show_usage
    
    print_success "Development environment setup completed successfully!"
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
        -c|--cluster)
            CLUSTER_NAME="$2"
            shift 2
            ;;
        -h|--help)
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  -n, --namespace NAMESPACE    Kubernetes namespace (default: homelab-autoscaler-system)"
            echo "  -r, --release RELEASE        Helm release name (default: homelab-autoscaler-dev)"
            echo "  -c, --cluster CLUSTER        K3d cluster name (default: homelab-autoscaler)"
            echo "  -h, --help                   Show this help message"
            echo ""
            echo "This script sets up a development environment where:"
            echo "  • Controller manager runs locally (outside cluster)"
            echo "  • Cluster autoscaler runs in-cluster"
            echo "  • Local service forwarder enables communication"
            echo ""
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