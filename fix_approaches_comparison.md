# Three Approaches to Fix Duplicate Metrics Registration

## Approach 1: Custom Registry Per Server Instance (Recommended)

**Concept**: Each server instance gets its own Prometheus registry instead of using the global one.

**Implementation**:
```go
type HomeClusterProviderServer struct {
    // ... existing fields ...
    registry prometheus.Registerer  // Add this field
}

func NewHomeClusterProviderServer(k8sClient client.Client, scheme *runtime.Scheme) *HomeClusterProviderServer {
    // Create custom registry for this instance
    registry := prometheus.NewRegistry()
    
    server := &HomeClusterProviderServer{
        // ... existing initialization ...
        registry: registry,
    }
    
    // Initialize metrics (same as before)
    server.groupScaleOperationsTotal = prometheus.NewCounterVec(...)
    // ... other metrics ...
    
    // Register with instance registry instead of global
    registry.MustRegister(
        server.groupScaleOperationsTotal,
        server.groupTargetSize,
        // ... other metrics ...
    )
    
    return server
}
```

**Pros**:
- Perfect test isolation - each test gets independent metrics
- No global state pollution
- Metrics still work normally within each instance
- Clean, predictable behavior

**Cons**:
- Metrics won't be exposed via global Prometheus endpoint in production
- Requires additional setup to expose metrics if needed

---

## Approach 2: sync.Once for Global Registration

**Concept**: Use sync.Once to ensure metrics are registered only once globally, regardless of how many server instances are created.

**Implementation**:
```go
var (
    metricsOnce sync.Once
    globalMetrics struct {
        groupScaleOperationsTotal *prometheus.CounterVec
        groupTargetSize           *prometheus.GaugeVec
        // ... other metrics ...
    }
)

func NewHomeClusterProviderServer(k8sClient client.Client, scheme *runtime.Scheme) *HomeClusterProviderServer {
    server := &HomeClusterProviderServer{
        // ... existing initialization ...
    }
    
    // Register metrics only once globally
    metricsOnce.Do(func() {
        globalMetrics.groupScaleOperationsTotal = prometheus.NewCounterVec(...)
        // ... initialize other metrics ...
        
        prometheus.MustRegister(
            globalMetrics.groupScaleOperationsTotal,
            globalMetrics.groupTargetSize,
            // ... other metrics ...
        )
    })
    
    // Point server metrics to global ones
    server.groupScaleOperationsTotal = globalMetrics.groupScaleOperationsTotal
    server.groupTargetSize = globalMetrics.groupTargetSize
    // ... other metrics ...
    
    return server
}
```

**Pros**:
- Simple implementation
- Metrics work with global Prometheus endpoint
- All server instances share the same metrics (might be desired)

**Cons**:
- No test isolation - all tests share the same metrics
- Metrics accumulate across test runs
- Global state makes testing unpredictable

---

## Approach 3: Registration Check

**Concept**: Check if metrics are already registered before attempting to register them.

**Implementation**:
```go
func NewHomeClusterProviderServer(k8sClient client.Client, scheme *runtime.Scheme) *HomeClusterProviderServer {
    server := &HomeClusterProviderServer{
        // ... existing initialization ...
    }
    
    // Initialize metrics
    server.groupScaleOperationsTotal = prometheus.NewCounterVec(...)
    // ... other metrics ...
    
    // Try to register, ignore if already registered
    err := prometheus.Register(server.groupScaleOperationsTotal)
    if err != nil {
        if _, ok := err.(prometheus.AlreadyRegisteredError); ok {
            // Get existing metric from registry
            existing := err.(prometheus.AlreadyRegisteredError).ExistingCollector
            server.groupScaleOperationsTotal = existing.(*prometheus.CounterVec)
        } else {
            panic(err) // Other registration errors should still panic
        }
    }
    // Repeat for other metrics...
    
    return server
}
```

**Pros**:
- Minimal code changes
- Works with global registry
- Handles the duplicate registration gracefully

**Cons**:
- Complex error handling for each metric
- All server instances share the same metrics (no isolation)
- Metrics accumulate across tests
- Type assertions are fragile

---

## Recommendation

**Approach 1 (Custom Registry)** is recommended because:
1. Provides proper test isolation
2. Clean, predictable behavior
3. Each server instance is independent
4. Easy to understand and maintain

For production use, if global metrics exposure is needed, we can add an optional parameter to use the global registry instead.