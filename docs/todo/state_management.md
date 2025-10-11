# Optimistic Locking Implementation Guide for Homelab-Autoscaler

## Table of Contents

1. [Optimistic Locking Overview](#optimistic-locking-overview)
2. [Current Implementation Analysis](#current-implementation-analysis)
3. [Detailed Implementation Guide](#detailed-implementation-guide)
4. [Specific Recommendations for Homelab-Autoscaler](#specific-recommendations-for-homelab-autoscaler)
5. [Implementation Patterns](#implementation-patterns)
6. [Testing and Validation](#testing-and-validation)
7. [Migration Guide](#migration-guide)
8. [Troubleshooting](#troubleshooting)

## Optimistic Locking Overview

### What is Optimistic Locking?

Optimistic locking is a concurrency control mechanism that assumes conflicts between concurrent operations are rare. Instead of locking resources during the entire operation, it validates that no other process has modified the resource before committing changes.

### Why Optimistic Locking for Kubernetes Controllers?

In Kubernetes controllers, optimistic locking is **essential** because:

1. **Multiple Controllers**: Different controllers may modify the same resource
2. **Concurrent Reconciliation**: The same controller may process multiple events for the same resource
3. **External Modifications**: Users or other systems may modify resources directly
4. **Resource Version Conflicts**: Kubernetes uses resource versions to detect concurrent modifications

### Benefits for Homelab-Autoscaler

- **Prevents Data Loss**: Ensures updates don't overwrite concurrent changes
- **Improves Reliability**: Reduces race conditions between controllers and gRPC server
- **Better Error Handling**: Provides clear feedback when conflicts occur
- **Scalability**: Allows multiple instances to run safely

### Trade-offs

**Advantages:**
- No deadlocks
- Better performance under low contention
- Natural fit for Kubernetes API patterns

**Disadvantages:**
- Requires retry logic
- May increase latency under high contention
- More complex error handling

## Current Implementation Analysis

### Existing Optimistic Locking Mechanisms

#### 1. Controller-Runtime Built-in Support

The project already uses controller-runtime, which provides optimistic locking through:

**Resource Versions** (in [`internal/controller/infra/group_controller.go:74`](internal/controller/infra/group_controller.go:74)):
```go
// Update the Group in the cluster
if err := r.Status().Update(ctx, group); err != nil {
    logger.Error(err, "Failed to update Group status", "group", req.Name)
    return ctrl.Result{}, err
}
```

**Status Subresource Usage** (in [`api/infra/v1alpha1/group_types.go:74`](api/infra/v1alpha1/group_types.go:74)):
```go
// +kubebuilder:subresource:status
```

#### 2. Current Patterns in Node Controller

**Basic Update Pattern** (in [`internal/controller/infra/node_controller.go:289`](internal/controller/infra/node_controller.go:289)):
```go
// Update the Node with the new status info
if err := r.Update(ctx, nodeCR); err != nil {
    logger.Error(err, "Failed to update Node with status condition", "node", nodeCR.Name)
    return err
}
```

**Patch-based Updates** (in [`internal/controller/infra/node_controller.go:183`](internal/controller/infra/node_controller.go:183)):
```go
// Create a patch with the new label
patch := client.MergeFrom(kubernetesNode.DeepCopy())
// Apply the patch
if err := r.Patch(ctx, kubernetesNode, patch); err != nil {
    logger.Error(err, "Failed to patch Kubernetes node with label", "node", nodeName, "label", labelKey)
    return err
}
```

#### 3. gRPC Server State Management

**Direct Client Updates** (in [`internal/grpcserver/server.go:370`](internal/grpcserver/server.go:370)):
```go
if err := s.Client.Update(ctx, node); err != nil {
    logger.Error(err, "failed to update node DesiredPowerState", "node", nodeName)
    return nil, status.Errorf(codes.Internal, "failed to update node %s DesiredPowerState to on: %v", nodeName, err)
}
```

### Current Issues and Gaps

1. **No Retry Logic**: Updates fail permanently on conflicts
2. **Mixed Update Patterns**: Some use `Update()`, others use `Status().Update()`
3. **No Conflict Detection**: No specific handling for resource version conflicts
4. **gRPC Race Conditions**: Multiple gRPC requests can conflict
5. **Status vs Spec Coordination**: No clear separation of concerns

## Detailed Implementation Guide

### 1. Resource Version Conflict Handling

#### Basic Retry Pattern

```go
func (r *NodeReconciler) updateNodeWithRetry(ctx context.Context, node *infrav1alpha1.Node, updateFunc func(*infrav1alpha1.Node)) error {
    logger := log.FromContext(ctx)
    
    const maxRetries = 3
    const baseDelay = 100 * time.Millisecond
    
    for attempt := 0; attempt < maxRetries; attempt++ {
        // Get fresh copy of the resource
        fresh := &infrav1alpha1.Node{}
        if err := r.Get(ctx, client.ObjectKeyFromObject(node), fresh); err != nil {
            return fmt.Errorf("failed to get fresh node: %w", err)
        }
        
        // Apply the update function
        updateFunc(fresh)
        
        // Attempt the update
        if err := r.Update(ctx, fresh); err != nil {
            if errors.IsConflict(err) {
                // Exponential backoff
                delay := baseDelay * time.Duration(1<<attempt)
                logger.Info("Resource conflict, retrying", 
                    "node", node.Name, 
                    "attempt", attempt+1, 
                    "delay", delay)
                
                select {
                case <-ctx.Done():
                    return ctx.Err()
                case <-time.After(delay):
                    continue
                }
            }
            return fmt.Errorf("failed to update node: %w", err)
        }
        
        logger.Info("Successfully updated node", "node", node.Name, "attempts", attempt+1)
        return nil
    }
    
    return fmt.Errorf("failed to update node after %d attempts", maxRetries)
}
```

#### Status-Only Updates with Retry

```go
func (r *NodeReconciler) updateNodeStatusWithRetry(ctx context.Context, node *infrav1alpha1.Node, updateFunc func(*infrav1alpha1.Node)) error {
    logger := log.FromContext(ctx)
    
    const maxRetries = 3
    
    for attempt := 0; attempt < maxRetries; attempt++ {
        // Get fresh copy for status update
        fresh := &infrav1alpha1.Node{}
        if err := r.Get(ctx, client.ObjectKeyFromObject(node), fresh); err != nil {
            return fmt.Errorf("failed to get fresh node for status update: %w", err)
        }
        
        // Apply status update
        updateFunc(fresh)
        
        // Update only status subresource
        if err := r.Status().Update(ctx, fresh); err != nil {
            if errors.IsConflict(err) {
                logger.Info("Status update conflict, retrying", 
                    "node", node.Name, 
                    "attempt", attempt+1)
                continue
            }
            return fmt.Errorf("failed to update node status: %w", err)
        }
        
        return nil
    }
    
    return fmt.Errorf("failed to update node status after %d attempts", maxRetries)
}
```

### 2. Advanced Retry Strategies

#### Exponential Backoff with Jitter

```go
type RetryConfig struct {
    MaxRetries  int
    BaseDelay   time.Duration
    MaxDelay    time.Duration
    Multiplier  float64
    Jitter      bool
}

func (r *NodeReconciler) updateWithAdvancedRetry(ctx context.Context, obj client.Object, updateFunc func(client.Object), config RetryConfig) error {
    logger := log.FromContext(ctx)
    
    for attempt := 0; attempt < config.MaxRetries; attempt++ {
        // Get fresh copy
        fresh := obj.DeepCopyObject().(client.Object)
        if err := r.Get(ctx, client.ObjectKeyFromObject(obj), fresh); err != nil {
            return fmt.Errorf("failed to get fresh object: %w", err)
        }
        
        // Apply update
        updateFunc(fresh)
        
        // Attempt update
        if err := r.Update(ctx, fresh); err != nil {
            if errors.IsConflict(err) {
                delay := r.calculateBackoffDelay(attempt, config)
                logger.Info("Conflict detected, backing off", 
                    "object", client.ObjectKeyFromObject(obj),
                    "attempt", attempt+1,
                    "delay", delay)
                
                select {
                case <-ctx.Done():
                    return ctx.Err()
                case <-time.After(delay):
                    continue
                }
            }
            return err
        }
        
        return nil
    }
    
    return fmt.Errorf("failed after %d attempts", config.MaxRetries)
}

func (r *NodeReconciler) calculateBackoffDelay(attempt int, config RetryConfig) time.Duration {
    delay := config.BaseDelay * time.Duration(math.Pow(config.Multiplier, float64(attempt)))
    
    if delay > config.MaxDelay {
        delay = config.MaxDelay
    }
    
    if config.Jitter {
        // Add ±25% jitter
        jitter := time.Duration(rand.Float64() * 0.5 * float64(delay))
        if rand.Float64() < 0.5 {
            delay -= jitter
        } else {
            delay += jitter
        }
    }
    
    return delay
}
```

### 3. Generation-Based Change Detection

#### Avoiding Infinite Reconciliation Loops

```go
func (r *NodeReconciler) shouldReconcile(ctx context.Context, node *infrav1alpha1.Node) bool {
    logger := log.FromContext(ctx)
    
    // Check if only status changed (Generation field comparison)
    if node.Generation == node.Status.ObservedGeneration {
        logger.V(1).Info("No spec changes detected, skipping reconciliation", 
            "node", node.Name,
            "generation", node.Generation,
            "observedGeneration", node.Status.ObservedGeneration)
        return false
    }
    
    return true
}

func (r *NodeReconciler) updateObservedGeneration(node *infrav1alpha1.Node) {
    // Update observed generation to match current generation
    node.Status.ObservedGeneration = node.Generation
}
```

### 4. Coordinated Spec and Status Updates

#### Separate Update Patterns

```go
func (r *NodeReconciler) reconcileNodePowerState(ctx context.Context, node *infrav1alpha1.Node) error {
    logger := log.FromContext(ctx)
    
    // 1. Update spec if needed (with retry)
    if node.Spec.DesiredPowerState != node.Status.PowerState {
        if err := r.updateNodeSpecWithRetry(ctx, node, func(n *infrav1alpha1.Node) {
            // Spec updates only
            n.Spec.DesiredPowerState = infrav1alpha1.PowerStateOn
        }); err != nil {
            return fmt.Errorf("failed to update node spec: %w", err)
        }
    }
    
    // 2. Update status separately (with retry)
    if err := r.updateNodeStatusWithRetry(ctx, node, func(n *infrav1alpha1.Node) {
        // Status updates only
        n.Status.Progress = infrav1alpha1.ProgressStartingUp
        n.Status.LastStartupTime = &metav1.Time{Time: time.Now()}
        r.updateObservedGeneration(n)
        
        // Update conditions
        condition := metav1.Condition{
            Type:               "Progressing",
            Status:             metav1.ConditionTrue,
            LastTransitionTime: metav1.Now(),
            Reason:             "PowerStateTransition",
            Message:            "Node is transitioning to desired power state",
        }
        meta.SetStatusCondition(&n.Status.Conditions, condition)
    }); err != nil {
        return fmt.Errorf("failed to update node status: %w", err)
    }
    
    return nil
}
```

## Specific Recommendations for Homelab-Autoscaler

### 1. gRPC Server Request Deduplication

#### Idempotency Keys

```go
type RequestTracker struct {
    mu       sync.RWMutex
    requests map[string]*RequestInfo
    ttl      time.Duration
}

type RequestInfo struct {
    Timestamp time.Time
    Result    interface{}
    Error     error
}

func (s *HomeClusterProviderServer) NodeGroupIncreaseSize(ctx context.Context, req *pb.NodeGroupIncreaseSizeRequest) (*pb.NodeGroupIncreaseSizeResponse, error) {
    logger := log.Log.WithName("grpc-server")
    
    // Generate idempotency key
    idempotencyKey := fmt.Sprintf("increase-%s-%d-%d", req.Id, req.Delta, time.Now().Unix()/60) // 1-minute window
    
    // Check for duplicate request
    if result, exists := s.requestTracker.GetResult(idempotencyKey); exists {
        logger.Info("Returning cached result for duplicate request", "key", idempotencyKey)
        if result.Error != nil {
            return nil, result.Error
        }
        return result.Result.(*pb.NodeGroupIncreaseSizeResponse), nil
    }
    
    // Process request with optimistic locking
    response, err := s.processNodeGroupIncreaseWithRetry(ctx, req)
    
    // Cache result
    s.requestTracker.SetResult(idempotencyKey, response, err)
    
    return response, err
}

func (s *HomeClusterProviderServer) processNodeGroupIncreaseWithRetry(ctx context.Context, req *pb.NodeGroupIncreaseSizeRequest) (*pb.NodeGroupIncreaseSizeResponse, error) {
    const maxRetries = 3
    
    for attempt := 0; attempt < maxRetries; attempt++ {
        if err := s.processNodeGroupIncrease(ctx, req); err != nil {
            if errors.IsConflict(err) {
                logger.Info("Conflict in gRPC operation, retrying", 
                    "nodeGroup", req.Id, 
                    "attempt", attempt+1)
                continue
            }
            return nil, err
        }
        return &pb.NodeGroupIncreaseSizeResponse{}, nil
    }
    
    return nil, status.Errorf(codes.Internal, "failed to increase node group size after %d attempts", maxRetries)
}
```

### 2. Controller Coordination Patterns

#### Shared State Management

```go
type StateManager struct {
    client client.Client
    scheme *runtime.Scheme
    mu     sync.RWMutex
    cache  map[string]*CachedState
}

type CachedState struct {
    ResourceVersion string
    LastUpdate      time.Time
    Data            interface{}
}

func (sm *StateManager) GetNodeWithLock(ctx context.Context, name string) (*infrav1alpha1.Node, func(), error) {
    sm.mu.Lock()
    defer sm.mu.Unlock()
    
    node := &infrav1alpha1.Node{}
    if err := sm.client.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
        return nil, nil, err
    }
    
    // Return unlock function
    unlock := func() {
        sm.mu.Unlock()
    }
    
    return node, unlock, nil
}
```

### 3. Cross-Controller State Management

#### Event-Driven Updates

```go
func (r *NodeReconciler) handleCrossControllerUpdate(ctx context.Context, node *infrav1alpha1.Node) error {
    logger := log.FromContext(ctx)
    
    // Check if update came from external source
    if node.Annotations["homelab-autoscaler.dev/updated-by"] != "node-controller" {
        logger.Info("External update detected, validating state", "node", node.Name)
        
        // Validate and potentially correct state
        if err := r.validateAndCorrectState(ctx, node); err != nil {
            return fmt.Errorf("failed to validate external update: %w", err)
        }
    }
    
    // Mark as processed by this controller
    return r.updateNodeWithRetry(ctx, node, func(n *infrav1alpha1.Node) {
        if n.Annotations == nil {
            n.Annotations = make(map[string]string)
        }
        n.Annotations["homelab-autoscaler.dev/updated-by"] = "node-controller"
        n.Annotations["homelab-autoscaler.dev/last-update"] = time.Now().Format(time.RFC3339)
    })
}
```

### 4. Operation-Level Locking for Critical Paths

#### Critical Section Protection

```go
type OperationLock struct {
    mu    sync.Mutex
    locks map[string]*sync.Mutex
}

func (ol *OperationLock) LockOperation(operationKey string) func() {
    ol.mu.Lock()
    if ol.locks == nil {
        ol.locks = make(map[string]*sync.Mutex)
    }
    
    lock, exists := ol.locks[operationKey]
    if !exists {
        lock = &sync.Mutex{}
        ol.locks[operationKey] = lock
    }
    ol.mu.Unlock()
    
    lock.Lock()
    return lock.Unlock
}

func (r *NodeReconciler) handleCriticalPowerStateChange(ctx context.Context, node *infrav1alpha1.Node) error {
    // Lock critical operation
    unlock := r.operationLock.LockOperation(fmt.Sprintf("power-state-%s", node.Name))
    defer unlock()
    
    // Perform critical state change with optimistic locking
    return r.updateNodeWithRetry(ctx, node, func(n *infrav1alpha1.Node) {
        // Critical power state logic
        n.Spec.DesiredPowerState = infrav1alpha1.PowerStateOn
    })
}
```

## Implementation Patterns

### 1. Resource Update Patterns with Retry Logic

#### Pattern 1: Simple Retry with Exponential Backoff

```go
func RetryOnConflict(ctx context.Context, fn func() error) error {
    backoff := wait.Backoff{
        Steps:    5,
        Duration: 100 * time.Millisecond,
        Factor:   2.0,
        Jitter:   0.1,
    }
    
    return wait.ExponentialBackoff(backoff, func() (bool, error) {
        err := fn()
        if err == nil {
            return true, nil
        }
        
        if errors.IsConflict(err) {
            return false, nil // Retry
        }
        
        return false, err // Don't retry
    })
}

// Usage
err := RetryOnConflict(ctx, func() error {
    return r.Update(ctx, node)
})
```

#### Pattern 2: Optimistic Update with Fresh Read

```go
func (r *NodeReconciler) OptimisticUpdate(ctx context.Context, obj client.Object, updateFn func(client.Object)) error {
    return RetryOnConflict(ctx, func() error {
        // Always get fresh copy
        fresh := obj.DeepCopyObject().(client.Object)
        if err := r.Get(ctx, client.ObjectKeyFromObject(obj), fresh); err != nil {
            return err
        }
        
        // Apply updates
        updateFn(fresh)
        
        // Attempt update
        return r.Update(ctx, fresh)
    })
}
```

### 2. Status vs Spec Update Coordination

#### Coordinated Update Pattern

```go
func (r *NodeReconciler) CoordinatedUpdate(ctx context.Context, node *infrav1alpha1.Node, specUpdate func(*infrav1alpha1.Node), statusUpdate func(*infrav1alpha1.Node)) error {
    // 1. Update spec first
    if specUpdate != nil {
        if err := r.OptimisticUpdate(ctx, node, func(obj client.Object) {
            specUpdate(obj.(*infrav1alpha1.Node))
        }); err != nil {
            return fmt.Errorf("spec update failed: %w", err)
        }
    }
    
    // 2. Update status separately
    if statusUpdate != nil {
        if err := r.OptimisticStatusUpdate(ctx, node, statusUpdate); err != nil {
            return fmt.Errorf("status update failed: %w", err)
        }
    }
    
    return nil
}

func (r *NodeReconciler) OptimisticStatusUpdate(ctx context.Context, node *infrav1alpha1.Node, updateFn func(*infrav1alpha1.Node)) error {
    return RetryOnConflict(ctx, func() error {
        fresh := &infrav1alpha1.Node{}
        if err := r.Get(ctx, client.ObjectKeyFromObject(node), fresh); err != nil {
            return err
        }
        
        updateFn(fresh)
        return r.Status().Update(ctx, fresh)
    })
}
```

### 3. Conflict Resolution Strategies

#### Strategy 1: Last Writer Wins with Validation

```go
func (r *NodeReconciler) ResolveConflict(ctx context.Context, current, desired *infrav1alpha1.Node) (*infrav1alpha1.Node, error) {
    logger := log.FromContext(ctx)
    
    // Get the latest version from API server
    latest := &infrav1alpha1.Node{}
    if err := r.Get(ctx, client.ObjectKeyFromObject(current), latest); err != nil {
        return nil, err
    }
    
    // Validate that the conflict is resolvable
    if !r.isConflictResolvable(current, latest, desired) {
        return nil, fmt.Errorf("unresolvable conflict detected")
    }
    
    // Merge changes intelligently
    resolved := r.mergeChanges(latest, desired)
    
    logger.Info("Resolved conflict", 
        "node", current.Name,
        "currentVersion", current.ResourceVersion,
        "latestVersion", latest.ResourceVersion)
    
    return resolved, nil
}

func (r *NodeReconciler) isConflictResolvable(current, latest, desired *infrav1alpha1.Node) bool {
    // Check if critical fields conflict
    if latest.Spec.DesiredPowerState != current.Spec.DesiredPowerState &&
       latest.Spec.DesiredPowerState != desired.Spec.DesiredPowerState {
        return false // Conflicting power state changes
    }
    
    return true
}

func (r *NodeReconciler) mergeChanges(latest, desired *infrav1alpha1.Node) *infrav1alpha1.Node {
    merged := latest.DeepCopy()
    
    // Merge spec changes
    if desired.Spec.DesiredPowerState != "" {
        merged.Spec.DesiredPowerState = desired.Spec.DesiredPowerState
    }
    
    // Merge status changes
    if len(desired.Status.Conditions) > 0 {
        for _, condition := range desired.Status.Conditions {
            meta.SetStatusCondition(&merged.Status.Conditions, condition)
        }
    }
    
    return merged
}
```

#### Strategy 2: Semantic Merge

```go
func (r *NodeReconciler) SemanticMerge(ctx context.Context, base, current, desired *infrav1alpha1.Node) (*infrav1alpha1.Node, error) {
    // Create three-way merge
    result := base.DeepCopy()
    
    // Apply changes from current that don't conflict with desired
    if current.Spec.DesiredPowerState != base.Spec.DesiredPowerState &&
       desired.Spec.DesiredPowerState == base.Spec.DesiredPowerState {
        result.Spec.DesiredPowerState = current.Spec.DesiredPowerState
    } else {
        result.Spec.DesiredPowerState = desired.Spec.DesiredPowerState
    }
    
    // Merge status conditions
    result.Status.Conditions = r.mergeConditions(base.Status.Conditions, current.Status.Conditions, desired.Status.Conditions)
    
    return result, nil
}
```

### 4. Error Handling and Logging Patterns

#### Comprehensive Error Context

```go
type ConflictError struct {
    Resource        client.Object
    Operation       string
    Attempt         int
    OriginalError   error
    ResourceVersion string
}

func (e *ConflictError) Error() string {
    return fmt.Sprintf("conflict in %s operation on %s (attempt %d, version %s): %v",
        e.Operation,
        client.ObjectKeyFromObject(e.Resource),
        e.Attempt,
        e.ResourceVersion,
        e.OriginalError)
}

func (r *NodeReconciler) handleUpdateError(ctx context.Context, err error, node *infrav1alpha1.Node, operation string, attempt int) error {
    logger := log.FromContext(ctx)
    
    if errors.IsConflict(err) {
        conflictErr := &ConflictError{
            Resource:        node,
            Operation:       operation,
            Attempt:         attempt,
            OriginalError:   err,
            ResourceVersion: node.ResourceVersion,
        }
        
        logger.Info("Resource conflict detected",
            "error", conflictErr,
            "node", node.Name,
            "operation", operation,
            "attempt", attempt,
            "resourceVersion", node.ResourceVersion)
        
        return conflictErr
    }
    
    return err
}
```

## Testing and Validation

### 1. Race Condition Testing

#### Concurrent Update Test

```go
func TestConcurrentNodeUpdates(t *testing.T) {
    ctx := context.Background()
    client := fake.NewClientBuilder().WithScheme(scheme).Build()
    
    // Create initial node
    node := &infrav1alpha1.Node{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "test-node",
            Namespace: "default",
        },
        Spec: infrav1alpha1.NodeSpec{
            DesiredPowerState: infrav1alpha1.PowerStateOff,
        },
    }
    require.NoError(t, client.Create(ctx, node))
    
    // Simulate concurrent updates
    var wg sync.WaitGroup
    errors := make(chan error, 10)
    
    for i := 0; i < 10; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            
            reconciler := &NodeReconciler{Client: client}
            err := reconciler.OptimisticUpdate(ctx, node, func(obj client.Object) {
                n := obj.(*infrav1alpha1.Node)
                n.Spec.DesiredPowerState = infrav1alpha1.PowerStateOn
                n.Status.Progress = infrav1alpha1.ProgressStartingUp
            })
            
            if err != nil {
                errors <- fmt.Errorf("goroutine %d failed: %w", id, err)
            }
        }(i)
    }
    
    wg.Wait()
    close(errors)
    
    // Check for errors
    var updateErrors []error
    for err := range errors {
        updateErrors = append(updateErrors, err)
    }
    
    // Some conflicts are expected, but all should eventually succeed
    assert.LessOrEqual(t, len(updateErrors), 5, "Too many update failures")
    
    // Verify final state
    final := &infrav1alpha1.Node{}
    require.NoError(t, client.Get(ctx, client.ObjectKeyFromObject(node), final))
    assert.Equal(t, infrav1alpha1.PowerStateOn, final.Spec.DesiredPowerState)
}
```

#### gRPC Concurrent Request Test

```go
func TestConcurrentGRPCRequests(t *testing.T) {
    ctx := context.Background()
    server := setupTestGRPCServer(t)
    
    // Create test node group
    group := createTestGroup(t, server.Client)
    
    var wg sync.WaitGroup
    results := make(chan error, 5)
    
    // Simulate concurrent increase requests
    for i := 0; i < 5; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            
            req := &pb.NodeGroupIncreaseSizeRequest{
                Id:    group.Name,
                Delta: 1,
            }
            
            _, err := server.NodeGroupIncreaseSize(ctx, req)
            results <- err
        }()
    }
    
    wg.Wait()
    close(results)
    
    // Verify that only one request succeeded (due to idempotency)
    successCount := 0
    for err := range results {
        if err == nil {
            successCount++
        }
    }
    
    assert.Equal(t, 1, successCount, "Only one concurrent request should succeed")
}
```

### 2. Performance Testing

#### Optimistic Locking Overhead Test

```go
func BenchmarkOptimisticUpdate(b *testing.B) {
    ctx := context.Background()
    client := setupBenchmarkClient(b)
    reconciler := &NodeReconciler{Client: client}
    
    node := createBenchmarkNode(b, client)
    
    b.ResetTimer()
    
    for i := 0; i < b.N; i++ {
        err := reconciler.OptimisticUpdate(ctx, node, func(obj client.Object) {
            n := obj.(*infrav1alpha1.Node)
            n.Status.Progress = infrav1alpha1.ProgressReady
        })
        
        if err != nil {
            b.Fatalf("Update failed: %v", err)
        }
    }
}

func BenchmarkDirectUpdate(b *testing.B) {
    ctx := context.Background()
    client := setupBenchmarkClient(b)
    reconciler := &NodeReconciler{Client: client}
    
    node := createBenchmarkNode(b, client)
    
    b.ResetTimer()
    
    for i := 0; i < b.N; i++ {
        node.Status.Progress = infrav1alpha1.ProgressReady
        err := reconciler.Status().Update(ctx, node)
        
        if err != nil {
            b.Fatalf("Update failed: %v", err)
        }
    }
}
```

### 3. Validation Strategies

#### State Consistency Validation

```go
func (r *NodeReconciler) ValidateStateConsistency(ctx context.Context, node *infrav1alpha1.Node) error {
    logger := log.FromContext(ctx)
    
    // Validate power state consistency
    if node.Spec.DesiredPowerState == infrav1alpha1.PowerStateOn &&
       node.Status.PowerState == infrav1alpha1.PowerStateOff &&
       node.Status.Progress == infrav1alpha1.ProgressReady {
        return fmt.
ProgressReady {
        return fmt.Errorf("inconsistent power state: desired=%s, actual=%s, progress=%s", 
            node.Spec.DesiredPowerState, node.Status.PowerState, node.Status.Progress)
    }
    
    // Validate transition states
    if node.Status.Progress == infrav1alpha1.ProgressStartingUp &&
       time.Since(node.Status.LastStartupTime.Time) > 10*time.Minute {
        logger.Warning("Node stuck in starting up state", "node", node.Name, "duration", time.Since(node.Status.LastStartupTime.Time))
        return fmt.Errorf("node stuck in starting up state for too long")
    }
    
    return nil
}
```

#### Integration Test Framework

```go
type OptimisticLockingTestSuite struct {
    suite.Suite
    client     client.Client
    reconciler *NodeReconciler
    ctx        context.Context
}

func (suite *OptimisticLockingTestSuite) SetupTest() {
    suite.ctx = context.Background()
    suite.client = fake.NewClientBuilder().WithScheme(scheme).Build()
    suite.reconciler = &NodeReconciler{
        Client: suite.client,
        Scheme: scheme,
    }
}

func (suite *OptimisticLockingTestSuite) TestOptimisticLockingScenarios() {
    // Test various optimistic locking scenarios
    scenarios := []struct {
        name     string
        testFunc func()
    }{
        {"ConcurrentSpecUpdates", suite.testConcurrentSpecUpdates},
        {"ConcurrentStatusUpdates", suite.testConcurrentStatusUpdates},
        {"MixedSpecStatusUpdates", suite.testMixedSpecStatusUpdates},
        {"ConflictResolution", suite.testConflictResolution},
    }
    
    for _, scenario := range scenarios {
        suite.Run(scenario.name, scenario.testFunc)
    }
}
```

## Migration Guide

### 1. Incremental Implementation Strategy

#### Phase 1: Add Retry Logic to Existing Controllers

**Week 1-2: Basic Retry Implementation**

1. **Update Node Controller** ([`internal/controller/infra/node_controller.go`](internal/controller/infra/node_controller.go)):

```go
// Replace existing update pattern at line 289
func (r *NodeReconciler) updateNodeWithRetry(ctx context.Context, node *infrav1alpha1.Node) error {
    return retry.RetryOnConflict(retry.DefaultRetry, func() error {
        // Get fresh copy
        fresh := &infrav1alpha1.Node{}
        if err := r.Get(ctx, client.ObjectKeyFromObject(node), fresh); err != nil {
            return err
        }
        
        // Apply your updates to fresh copy
        fresh.Status.Progress = node.Status.Progress
        fresh.Status.LastStartupTime = node.Status.LastStartupTime
        fresh.Status.Conditions = node.Status.Conditions
        
        return r.Update(ctx, fresh)
    })
}
```

2. **Update Group Controller** ([`internal/controller/infra/group_controller.go`](internal/controller/infra/group_controller.go)):

```go
// Replace status update at line 74
func (r *GroupReconciler) updateGroupStatusWithRetry(ctx context.Context, group *infrav1alpha1.Group) error {
    return retry.RetryOnConflict(retry.DefaultRetry, func() error {
        fresh := &infrav1alpha1.Group{}
        if err := r.Get(ctx, client.ObjectKeyFromObject(group), fresh); err != nil {
            return err
        }
        
        fresh.Status.Conditions = group.Status.Conditions
        return r.Status().Update(ctx, fresh)
    })
}
```

#### Phase 2: Enhance gRPC Server

**Week 3-4: gRPC Optimistic Locking**

1. **Add Request Deduplication** ([`internal/grpcserver/server.go`](internal/grpcserver/server.go)):

```go
// Add to HomeClusterProviderServer struct
type HomeClusterProviderServer struct {
    // ... existing fields
    requestTracker *RequestTracker
    mu             sync.RWMutex
}

// Update NodeGroupIncreaseSize method at line 324
func (s *HomeClusterProviderServer) NodeGroupIncreaseSize(ctx context.Context, req *pb.NodeGroupIncreaseSizeRequest) (*pb.NodeGroupIncreaseSizeResponse, error) {
    // Add idempotency check
    idempotencyKey := fmt.Sprintf("increase-%s-%d", req.Id, req.Delta)
    
    s.mu.Lock()
    if result, exists := s.requestTracker.Get(idempotencyKey); exists {
        s.mu.Unlock()
        return result, nil
    }
    s.mu.Unlock()
    
    // Process with retry logic
    return s.processWithRetry(ctx, req, idempotencyKey)
}
```

#### Phase 3: Advanced Patterns

**Week 5-6: Generation-based Detection and Coordination**

1. **Add ObservedGeneration to Status** ([`api/infra/v1alpha1/node_types.go`](api/infra/v1alpha1/node_types.go)):

```go
// Add to NodeStatus struct at line 73
type NodeStatus struct {
    // ... existing fields
    
    // ObservedGeneration reflects the generation of the most recently observed Node.
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}
```

2. **Update Reconcile Logic**:

```go
func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // ... existing code
    
    // Check if reconciliation is needed
    if !r.shouldReconcile(ctx, node) {
        return ctrl.Result{}, nil
    }
    
    // ... rest of reconciliation logic
    
    // Update observed generation
    return r.updateStatusWithObservedGeneration(ctx, node)
}
```

### 2. Backward Compatibility Considerations

#### Configuration-Based Rollout

```go
type OptimisticLockingConfig struct {
    Enabled                bool          `json:"enabled"`
    MaxRetries            int           `json:"maxRetries"`
    BaseDelay             time.Duration `json:"baseDelay"`
    EnableRequestTracking bool          `json:"enableRequestTracking"`
}

func (r *NodeReconciler) updateWithConfig(ctx context.Context, node *infrav1alpha1.Node, config OptimisticLockingConfig) error {
    if !config.Enabled {
        // Fall back to original behavior
        return r.Update(ctx, node)
    }
    
    // Use optimistic locking
    return r.updateNodeWithRetry(ctx, node)
}
```

#### Feature Flags

```go
const (
    FeatureFlagOptimisticLocking = "ENABLE_OPTIMISTIC_LOCKING"
    FeatureFlagRequestTracking   = "ENABLE_REQUEST_TRACKING"
)

func (s *HomeClusterProviderServer) isOptimisticLockingEnabled() bool {
    return os.Getenv(FeatureFlagOptimisticLocking) == "true"
}
```

### 3. Rollback Strategies

#### Graceful Degradation

```go
func (r *NodeReconciler) updateWithFallback(ctx context.Context, node *infrav1alpha1.Node) error {
    // Try optimistic locking first
    if err := r.updateNodeWithRetry(ctx, node); err != nil {
        logger := log.FromContext(ctx)
        logger.Warning("Optimistic locking failed, falling back to direct update", 
            "node", node.Name, "error", err)
        
        // Fall back to direct update
        return r.Update(ctx, node)
    }
    
    return nil
}
```

#### Monitoring and Alerting

```go
type OptimisticLockingMetrics struct {
    ConflictCount     prometheus.Counter
    RetryCount        prometheus.Counter
    FallbackCount     prometheus.Counter
    SuccessfulUpdates prometheus.Counter
}

func (r *NodeReconciler) recordConflict(operation string) {
    r.metrics.ConflictCount.WithLabelValues(operation).Inc()
}

func (r *NodeReconciler) recordRetry(operation string, attempt int) {
    r.metrics.RetryCount.WithLabelValues(operation, fmt.Sprintf("%d", attempt)).Inc()
}
```

## Troubleshooting

### 1. Common Issues and Solutions

#### Issue 1: High Conflict Rate

**Symptoms:**
- Frequent "resource version conflict" errors
- High retry counts in logs
- Slow reconciliation performance

**Diagnosis:**
```bash
# Check controller logs for conflict patterns
kubectl logs -n homelab-autoscaler-system deployment/homelab-autoscaler-controller | grep "conflict"

# Monitor retry metrics
kubectl port-forward -n homelab-autoscaler-system svc/homelab-autoscaler-metrics 8080:8080
curl http://localhost:8080/metrics | grep optimistic_locking
```

**Solutions:**
1. **Reduce Reconciliation Frequency:**
```go
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&infrav1alpha1.Node{}).
        WithOptions(controller.Options{
            MaxConcurrentReconciles: 1, // Reduce concurrency
        }).
        Complete(r)
}
```

2. **Implement Backoff Strategy:**
```go
func (r *NodeReconciler) calculateAdaptiveBackoff(conflictCount int) time.Duration {
    baseDelay := 100 * time.Millisecond
    maxDelay := 5 * time.Second
    
    delay := baseDelay * time.Duration(math.Pow(2, float64(conflictCount)))
    if delay > maxDelay {
        delay = maxDelay
    }
    
    return delay
}
```

#### Issue 2: Infinite Reconciliation Loops

**Symptoms:**
- Controller continuously reconciling the same resource
- High CPU usage
- No actual state changes

**Diagnosis:**
```go
func (r *NodeReconciler) debugReconciliationLoop(ctx context.Context, node *infrav1alpha1.Node) {
    logger := log.FromContext(ctx)
    logger.Info("Reconciliation debug info",
        "node", node.Name,
        "generation", node.Generation,
        "observedGeneration", node.Status.ObservedGeneration,
        "resourceVersion", node.ResourceVersion,
        "lastUpdate", node.Status.LastStartupTime)
}
```

**Solutions:**
1. **Implement Generation Checking:**
```go
func (r *NodeReconciler) shouldReconcile(node *infrav1alpha1.Node) bool {
    return node.Generation != node.Status.ObservedGeneration
}
```

2. **Separate Status Updates:**
```go
func (r *NodeReconciler) updateStatusOnly(ctx context.Context, node *infrav1alpha1.Node) error {
    // Only update status subresource to avoid triggering spec changes
    return r.Status().Update(ctx, node)
}
```

#### Issue 3: gRPC Request Conflicts

**Symptoms:**
- Multiple gRPC requests failing with conflicts
- Inconsistent cluster state
- Client timeout errors

**Diagnosis:**
```go
func (s *HomeClusterProviderServer) logRequestConflict(ctx context.Context, req interface{}, err error) {
    logger := log.Log.WithName("grpc-server")
    logger.Error(err, "gRPC request conflict",
        "request", fmt.Sprintf("%T", req),
        "timestamp", time.Now(),
        "context", ctx.Value("request-id"))
}
```

**Solutions:**
1. **Implement Request Deduplication:**
```go
func (s *HomeClusterProviderServer) deduplicateRequest(ctx context.Context, key string, fn func() (interface{}, error)) (interface{}, error) {
    s.mu.Lock()
    if result, exists := s.requestCache[key]; exists {
        s.mu.Unlock()
        return result, nil
    }
    s.mu.Unlock()
    
    result, err := fn()
    
    s.mu.Lock()
    s.requestCache[key] = result
    s.mu.Unlock()
    
    return result, err
}
```

### 2. Debugging Tools and Techniques

#### Resource Version Tracking

```go
type ResourceVersionTracker struct {
    mu       sync.RWMutex
    versions map[string][]string
}

func (rvt *ResourceVersionTracker) Track(obj client.Object) {
    rvt.mu.Lock()
    defer rvt.mu.Unlock()
    
    key := client.ObjectKeyFromObject(obj).String()
    rvt.versions[key] = append(rvt.versions[key], obj.GetResourceVersion())
}

func (rvt *ResourceVersionTracker) GetHistory(obj client.Object) []string {
    rvt.mu.RLock()
    defer rvt.mu.RUnlock()
    
    key := client.ObjectKeyFromObject(obj).String()
    return rvt.versions[key]
}
```

#### Conflict Analysis

```go
func (r *NodeReconciler) analyzeConflict(ctx context.Context, original, current *infrav1alpha1.Node) {
    logger := log.FromContext(ctx)
    
    // Compare resource versions
    logger.Info("Conflict analysis",
        "node", original.Name,
        "originalVersion", original.ResourceVersion,
        "currentVersion", current.ResourceVersion,
        "originalGeneration", original.Generation,
        "currentGeneration", current.Generation)
    
    // Compare specific fields
    if original.Spec.DesiredPowerState != current.Spec.DesiredPowerState {
        logger.Info("Power state conflict detected",
            "original", original.Spec.DesiredPowerState,
            "current", current.Spec.DesiredPowerState)
    }
    
    if !reflect.DeepEqual(original.Status.Conditions, current.Status.Conditions) {
        logger.Info("Conditions conflict detected",
            "originalCount", len(original.Status.Conditions),
            "currentCount", len(current.Status.Conditions))
    }
}
```

### 3. Performance Monitoring

#### Metrics Collection

```go
var (
    optimisticLockingConflicts = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Name: "homelab_autoscaler_optimistic_locking_conflicts_total",
            Help: "Total number of optimistic locking conflicts",
        },
        []string{"controller", "resource", "operation"},
    )
    
    optimisticLockingRetries = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name: "homelab_autoscaler_optimistic_locking_retries",
            Help: "Number of retries for optimistic locking operations",
            Buckets: prometheus.LinearBuckets(0, 1, 10),
        },
        []string{"controller", "resource", "operation"},
    )
)

func init() {
    prometheus.MustRegister(optimisticLockingConflicts)
    prometheus.MustRegister(optimisticLockingRetries)
}
```

#### Health Checks

```go
func (r *NodeReconciler) healthCheck(ctx context.Context) error {
    // Check if conflict rate is too high
    conflictRate := r.getConflictRate()
    if conflictRate > 0.5 { // More than 50% conflicts
        return fmt.Errorf("high conflict rate detected: %.2f", conflictRate)
    }
    
    // Check if retry count is excessive
    avgRetries := r.getAverageRetries()
    if avgRetries > 3 {
        return fmt.Errorf("excessive retry count: %.2f", avgRetries)
    }
    
    return nil
}
```

## Conclusion

This comprehensive guide provides a complete implementation strategy for optimistic locking in the homelab-autoscaler project. The key takeaways are:

1. **Incremental Implementation**: Start with basic retry logic and gradually add advanced features
2. **Separation of Concerns**: Keep spec and status updates separate
3. **Proper Error Handling**: Implement comprehensive conflict detection and resolution
4. **Testing Strategy**: Include race condition and performance testing
5. **Monitoring**: Track conflicts and performance metrics
6. **Graceful Degradation**: Provide fallback mechanisms for reliability

By following this guide, the homelab-autoscaler will achieve robust state management with proper concurrency control, ensuring data consistency and system reliability in multi-controller environments.

### Next Steps

1. **Implement Phase 1**: Add basic retry logic to existing controllers
2. **Add Monitoring**: Implement metrics collection for conflict tracking
3. **Test Thoroughly**: Run concurrent update tests and performance benchmarks
4. **Deploy Incrementally**: Use feature flags for gradual rollout
5. **Monitor and Tune**: Adjust retry strategies based on production metrics

### References

- [Kubernetes API Conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md)
- [Controller-Runtime Client Documentation](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/client)
- [Optimistic Concurrency Control](https://en.wikipedia.org/wiki/Optimistic_concurrency_control)
- [Kubernetes Resource Versions](https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions)