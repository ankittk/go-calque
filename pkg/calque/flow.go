package calque

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"sync"
)

// ConcurrencyUnlimited disables concurrency limits, allowing unlimited handler goroutines.
// This provides maximum performance but may exhaust resources under high load.
// Use for development and moderate load scenarios.
const ConcurrencyUnlimited = 0

// ConcurrencyAuto automatically calculates concurrency limits based on CPU cores.
// Uses runtime.GOMAXPROCS(0) * CPUMultiplier to determine the maximum concurrent
// handler goroutines. Balances performance with resource protection.
// Use for production HTTP APIs and high-load scenarios.
const ConcurrencyAuto = -1

// DefaultCPUMultiplier provides a conservative default for mixed I/O workloads.
//
// On a 4-core system: 4 * 50 = 200 concurrent handlers.
// Since workload depends on the individual flow this is just a starting point,
// Increase for heavily I/O-bound workloads or decrease for CPU-intensive tasks.
//
// Example usage:
//
//	config := FlowConfig{
//		MaxConcurrent: ConcurrencyAuto,
//		CPUMultiplier: 100, // More aggressive for AI API calls
//	}
const DefaultCPUMultiplier = 50

// FlowConfig configures flow concurrency behavior and resource limits.
//
// MaxConcurrent controls the maximum number of handler goroutines that can run
// simultaneously across all flow executions. Use ConcurrencyUnlimited for no limits,
// ConcurrencyAuto for CPU-based limits, or a positive integer for fixed limits.
//
// CPUMultiplier is used when MaxConcurrent = ConcurrencyAuto to calculate the
// actual limit: runtime.GOMAXPROCS(0) * CPUMultiplier. Higher values allow more
// concurrency for I/O-bound workloads.
//
// Example configurations:
//
//	// Default: unlimited concurrency (best for development)
//	flow := calque.NewFlow()
//
//	// Auto-scaling based on CPU cores (good for production)
//	flow := calque.NewFlow(calque.FlowConfig{
//		MaxConcurrent: calque.ConcurrencyAuto,
//		CPUMultiplier: 75, // Adjust based on workload
//	})
//
//	// Fixed limit (precise resource control)
//	flow := calque.NewFlow(calque.FlowConfig{MaxConcurrent: 100})
type FlowConfig struct {
	MaxConcurrent int // ConcurrencyUnlimited, ConcurrencyAuto, or positive integer
	CPUMultiplier int // multiplier for GOMAXPROCS (used when MaxConcurrent = ConcurrencyAuto)
}

// Flow is the core flow orchestration primitive
type Flow struct {
	handlers []Handler
	sem      chan struct{} // nil = unlimited concurrency
}

// NewFlow creates a new flow with optional concurrency configuration.
//
// Input: optional FlowConfig for concurrency control
// Output: *Flow ready for handler registration
// Behavior: Creates flow with specified or default concurrency limits
//
// With no config, uses unlimited concurrency (good for development and moderate load).
// With config, applies semaphore-based goroutine limiting for resource protection.
// Each handler in the flow runs in its own goroutine, connected by io.Pipe.
//
// The semaphore limits the total number of handler goroutines across ALL flow
// executions, preventing resource exhaustion under high concurrent load.
//
// Example usage:
//
//	// Default: unlimited concurrency
//	flow := calque.NewFlow()
//
//	// Auto-scaling: limits based on CPU cores
//	flow := calque.NewFlow(calque.FlowConfig{
//		MaxConcurrent: calque.ConcurrencyAuto,
//		CPUMultiplier: 100, // 100x CPU cores
//	})
//
//	// Fixed limit: precise control
//	flow := calque.NewFlow(calque.FlowConfig{MaxConcurrent: 50})
func NewFlow(configs ...FlowConfig) *Flow {
	var config FlowConfig
	if len(configs) > 0 {
		config = configs[0]
	} else {
		// Default: unlimited concurrency
		config = FlowConfig{
			MaxConcurrent: ConcurrencyUnlimited,
			CPUMultiplier: DefaultCPUMultiplier,
		}
	}

	var sem chan struct{}
	switch config.MaxConcurrent {
	case ConcurrencyUnlimited:
		// Unlimited concurrency
		sem = nil
	case ConcurrencyAuto:
		// Auto-calculate based on CPU
		multiplier := config.CPUMultiplier
		if multiplier <= 0 {
			multiplier = DefaultCPUMultiplier
		}
		limit := runtime.GOMAXPROCS(0) * multiplier
		sem = make(chan struct{}, limit)
	default:
		// Fixed limit
		if config.MaxConcurrent > 0 {
			sem = make(chan struct{}, config.MaxConcurrent)
		}
	}

	return &Flow{sem: sem}
}

// Use adds a handler to the flow chain.
//
// Input: calque.Handler to add to the processing chain
// Output: *Flow (fluent interface for chaining)
// Behavior: Appends handler to the flow chain
//
// Handlers are executed in the order they are added. Each handler runs in its own
// goroutine and connects to the next handler via io.Pipe for streaming data flow.
// The flow supports unlimited handler chaining.
//
// Example:
//
//	flow := calque.NewFlow().
//		Use(logger.Print("INPUT")).
//		Use(ai.Agent(client)).
//		Use(logger.Print("OUTPUT"))
func (f *Flow) Use(handler Handler) *Flow {
	f.handlers = append(f.handlers, handler)
	return f
}

// UseFunc adds a function as a handler using the HandlerFunc adapter.
//
// Input: HandlerFunc - function matching the handler signature
// Output: *Flow (fluent interface for chaining)
// Behavior: Wraps function as Handler and adds to flow
//
// Convenience method for adding functions directly without explicit HandlerFunc wrapping.
// The function must match the signature: func(*Request, *Response) error
//
// Example:
//
//	flow := calque.NewFlow().
//		UseFunc(func(req *calque.Request, res *calque.Response) error {
//			// Custom processing logic
//			return calque.Write(res, "processed")
//		})
func (f *Flow) UseFunc(fn HandlerFunc) *Flow {
	return f.Use(fn)
}

// ServeFlow implements the Handler interface, enabling flow composability.
//
// Input: *Request containing context and input data stream
// Output: error if flow execution fails
// Behavior: STREAMING - executes the entire flow as a single handler
//
// This allows flows to be used as handlers in other flows, enabling
// true composability where complex sub-flows can be embedded anywhere
// a handler is expected.
//
// Example:
//
//	subFlow := calque.NewFlow().Use(handler1).Use(handler2)
//	mainFlow := calque.NewFlow().Use(subFlow).Use(handler3)
func (f *Flow) ServeFlow(req *Request, res *Response) error {
	return f.runWithStreaming(req.Context, req.Data, res.Data)
}

// Run executes the flow with streaming data flow and concurrent handler processing.
//
// Input: context.Context for cancellation, input data (any type), output pointer (any type)
// Output: error if flow execution fails
// Behavior: CONCURRENT - each handler runs in its own goroutine connected by io.Pipe
//
// The flow creates a chain of handler goroutines connected by io.Pipe instances.
// Data flows through the chain as it's processed, enabling true streaming with constant
// memory usage regardless of input size. If concurrency limiting is configured,
// handler goroutines acquire semaphore slots before execution.
//
// Input is automatically converted to io.Reader, output is parsed from final io.Writer.
// Context cancellation propagates through all handlers for clean shutdown.
// Flow execution fails if any handler returns an error.
//
// Example:
//
//	var result string
//	err := flow.Run(context.Background(), "input data", &result)
//	if err != nil {
//		log.Fatal(err)
//	}
//	fmt.Println("Output:", result)
func (f *Flow) Run(ctx context.Context, input any, output any) error {
	if len(f.handlers) == 0 {
		// No handlers, just copy input to output with conversion
		return f.copyInputToOutput(input, output)
	}

	// 1. Convert input (any) -> io.Reader
	reader, err := f.inputToReader(input)
	if err != nil {
		return err
	}

	// 2. Execute flow with pure streaming I/O
	var outputBuffer bytes.Buffer
	if err := f.runWithStreaming(ctx, reader, &outputBuffer); err != nil {
		return err
	}

	// 3. Convert io.Reader -> output (any)
	return f.readerToOutput(&outputBuffer, output)
}

// runWithStreaming executes the flow with pure streaming I/O (no conversions).
//
// Input: context.Context for cancellation, io.Reader for input stream, io.Writer for output
// Output: error if flow execution fails
// Behavior: STREAMING - each handler runs in its own goroutine connected by io.Pipe
//
// This is the core streaming execution logic separated from conversion concerns.
// Enables flow composability by working with raw streaming I/O interfaces.
func (f *Flow) runWithStreaming(ctx context.Context, input io.Reader, output io.Writer) error {
	if len(f.handlers) == 0 {
		// No handlers, just copy input to output
		_, err := io.Copy(output, input)
		return err
	}

	// Create a chain of pipes between handlers
	pipes := make([]struct {
		r *PipeReader
		w *PipeWriter
	}, len(f.handlers))

	// Creates pipe pairs (r, w) for each handler - these connect handlers together
	for i := 0; i < len(f.handlers); i++ {
		pipes[i].r, pipes[i].w = Pipe()
	}

	// Creates inputReader for the first handler's input
	inputR, inputW := io.Pipe()                    // Create a pipe for input
	inputReader := &PipeReader{PipeReader: inputR} // Wraps the input reader
	go func() {
		defer inputW.Close()
		io.Copy(inputW, input) // Copy input reader to pipe writer
	}()

	// Sets finalReader to read the last handler's output
	finalReader := pipes[len(pipes)-1].r

	//  Runs all handlers concurrently in goroutines for streaming
	//  Handler1: [========]
	//  Handler2:   [========]
	//  Handler3:     [========]
	var wg sync.WaitGroup
	errCh := make(chan error, len(f.handlers)+2) //create error chan with small extra buffer

	for i, handler := range f.handlers {
		wg.Add(1)
		go func(idx int, h Handler) {
			// Acquire semaphore if limiting is enabled
			if f.sem != nil {
				select {
				case f.sem <- struct{}{}: // Try to acquire semaphore slot
					defer func() { <-f.sem }() // Release when this handler completes
				case <-ctx.Done():
					errCh <- ctx.Err() // Flow cancelled while waiting for semaphore
					wg.Done()
					return
				}
			}

			defer wg.Done()
			defer pipes[idx].w.Close()

			var reader io.Reader
			if idx == 0 {
				reader = inputReader // Handler 0 reads from inputReader
			} else {
				reader = pipes[idx-1].r // Subsequent handlers read from the previous pipe's reader
			}

			// Each handler writes to its own pipe writer, which feeds the next handler
			req := &Request{Context: ctx, Data: reader}
			res := &Response{Data: pipes[idx].w}
			if err := h.ServeFlow(req, res); err != nil {
				errCh <- err
			}
		}(i, handler)
	}

	// Consume final output in background
	outputDone := make(chan error, 2)
	go func() {
		_, err := io.Copy(output, finalReader)
		outputDone <- err
	}()

	// Waits for either: context cancellation, handler error, or all handlers complete
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	case <-done:
		// Wait for output collection to complete
		return <-outputDone
	}
}
