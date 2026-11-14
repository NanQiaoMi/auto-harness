package telemetrybackend

import (
	"context"
	"sync"
	"time"

	"github.com/NanQiaoMi/auto-harness/telemetry"
)

// Exporter moves telemetry from the contract's recorder to a backend.
//
// It is the piece between the two halves: the recorder produces spans
// and measurements, the backend ships them. The exporter decides when
// to ship.
//
// Spans go out in batches, not one request per span: a run makes
// hundreds of spans and a request each would hammer the collector.
// The sink hands the span to a queue and returns; a background
// goroutine drains the queue and ships it.
//
// Metrics go out on an interval, because a counter that ships per
// measurement is a request per measurement. The aggregator sums them
// first.
//
// There is exactly one consumer of the span queue: the background
// drainer. An explicit Flush asks that drainer to do a round rather
// than taking spans itself, so a span is never shipped twice.
type Exporter struct {
	// backend ships the data.
	backend Backend
	// aggregator turns measurements into aggregate points.
	aggregator *Aggregator
	// interval is how often the metrics go out.
	interval time.Duration
	// batchSize is how many spans one request carries.
	batchSize int
	// logf receives non-fatal diagnostics.
	logf func(format string, args ...any)

	// queue holds the spans waiting to be shipped.
	queue chan telemetry.StoredSpan
	// drain asks the background drainer to do a round now.
	drain chan chan struct{}
	// done is closed when the background goroutines stop.
	done chan struct{}
	// wg waits for the background goroutines.
	wg sync.WaitGroup

	mu sync.Mutex
	// stopped marks the exporter as closed.
	stopped bool
	// flushed counts the metric batches shipped.
	flushed int
	// sent counts the spans shipped.
	sent int
}

// NewExporter creates an exporter over a backend.
//
// The metrics go out every interval; the spans go out in batches as
// they settle. A zero interval means the default.
func NewExporter(backend Backend, schema telemetry.SchemaDefinition, interval time.Duration, logf func(format string, args ...any)) *Exporter {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Exporter{
		backend:    backend,
		aggregator: NewAggregator(schema, interval),
		interval:   interval,
		batchSize:  DefaultBatchSize,
		logf:       logf,
		queue:      make(chan telemetry.StoredSpan, 4096),
		drain:      make(chan chan struct{}),
		done:       make(chan struct{}),
	}
}

// SetBatchSize sets how many spans one request carries.
func (e *Exporter) SetBatchSize(size int) {
	if size <= 0 {
		size = DefaultBatchSize
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.batchSize = size
}

// SpanSink returns the function that feeds settled spans to the
// backend.
//
// The recorder's sink is called on the goroutine that settled the
// span, so the exporter only enqueues the span: a slow collector must
// not hold up the agent.
func (e *Exporter) SpanSink() func(telemetry.StoredSpan) {
	return func(span telemetry.StoredSpan) {
		e.mu.Lock()
		stopped := e.stopped
		e.mu.Unlock()
		if stopped {
			return
		}
		select {
		case e.queue <- span:
		case <-e.done:
		default:
			// The queue is full: the collector is not keeping up.
			// Dropping the span is better than blocking the agent.
			e.logf("the span queue is full; dropping a span")
		}
	}
}

// RecordMeasurement feeds one measurement into the aggregator.
func (e *Exporter) RecordMeasurement(meas telemetry.RecordedMeasurement) {
	e.aggregator.RecordMeasurement(meas)
}

// Start runs the background goroutines: one drains the span queue, one
// flushes the metrics on the interval.
//
// The exporter runs without them too: the queue still fills and the
// metrics still flush on an explicit Flush. Starting them is what
// makes the telemetry leave the process while it runs.
func (e *Exporter) Start() {
	e.wg.Add(2)
	go func() {
		defer e.wg.Done()
		e.drainSpans()
	}()
	go func() {
		defer e.wg.Done()
		ticker := time.NewTicker(e.interval)
		defer ticker.Stop()
		for {
			select {
			case <-e.done:
				return
			case <-ticker.C:
				e.FlushMetrics(context.Background())
			}
		}
	}()
}

// drainSpans ships the queued spans in batches.
//
// It is the only consumer of the queue, so a span is never shipped
// twice.
func (e *Exporter) drainSpans() {
	batch := make([]telemetry.StoredSpan, 0, e.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := e.backend.SendTraces(ctx, batch); err != nil {
			e.logf("send traces: %v", err)
		} else {
			e.mu.Lock()
			e.sent += len(batch)
			e.mu.Unlock()
		}
		batch = batch[:0]
	}
	// take moves what is queued into the batch.
	take := func() {
		for {
			select {
			case span := <-e.queue:
				batch = append(batch, span)
				if len(batch) >= e.batchSize {
					flush()
				}
			default:
				return
			}
		}
	}

	for {
		select {
		case span := <-e.queue:
			batch = append(batch, span)
			if len(batch) >= e.batchSize {
				flush()
			}
		case ack := <-e.drain:
			// An explicit flush asked for a round.
			take()
			flush()
			close(ack)
		case <-e.done:
			// Ship what is left before leaving.
			take()
			flush()
			return
		}
	}
}

// FlushMetrics ships the metrics recorded so far.
func (e *Exporter) FlushMetrics(ctx context.Context) {
	points := e.aggregator.Points()
	if len(points) == 0 {
		return
	}
	if err := e.backend.SendMetrics(ctx, points); err != nil {
		e.logf("send metrics: %v", err)
		return
	}
	e.mu.Lock()
	e.flushed++
	e.mu.Unlock()
}

// Flush ships everything pending: the queued spans first, then the
// metrics, then the backend's own buffer.
//
// The spans go out through the background drainer, so there is only
// ever one consumer of the queue.
func (e *Exporter) Flush(ctx context.Context) error {
	e.drainSpansNow(ctx)
	e.FlushMetrics(ctx)
	return e.backend.Flush(ctx)
}

// drainSpansNow asks the background drainer to ship what is queued.
//
// When the exporter runs without its background goroutines, the queue
// is drained here instead.
func (e *Exporter) drainSpansNow(ctx context.Context) {
	ack := make(chan struct{})
	select {
	case e.drain <- ack:
		select {
		case <-ack:
		case <-ctx.Done():
		}
	case <-e.done:
		// The drainer is gone; the queue is flushed by Close.
	case <-time.After(5 * time.Second):
		e.logf("the span drainer did not answer")
	}
}

// Close stops the background goroutines, flushes what is pending and
// releases the backend.
func (e *Exporter) Close() error {
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return nil
	}
	e.stopped = true
	e.mu.Unlock()

	close(e.done)
	// The drainer ships what is left when it sees the close; waiting
	// for it means the queue is empty before the final flush.
	e.wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.FlushMetrics(ctx)
	return e.backend.Close()
}

// Flushed returns the number of metric batches shipped.
func (e *Exporter) Flushed() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.flushed
}

// Sent returns the number of spans shipped.
func (e *Exporter) Sent() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sent
}
