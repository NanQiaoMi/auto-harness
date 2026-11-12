package telemetry

// The harness's own telemetry vocabulary.
//
// The names follow pi's convention: a package owns a `pi.<package>.*`
// namespace, and adapters translate them to backend conventions without
// changing the emitted vocabulary. The harness uses `pi.harness.*` so
// its spans do not collide with pi-ai's `pi.ai.*`.
//
// The policy the schema encodes: attribute values are primitives only,
// and nothing here carries a prompt, a completion, a tool argument, a
// file path or a provider payload. Only facts the business code
// already has are recorded, and every attribute is low cardinality.

// harnessSchemaVersion is the version of the harness schema.
const harnessSchemaVersion = 1

// Schema is the harness's telemetry vocabulary.
var Schema = SchemaDefinition{
	Version: harnessSchemaVersion,
	Spans: map[string]SpanDefinition{
		"pi.harness.session": {
			Description: "One harness session, from startup to shutdown.",
			Parents:     ParentDefinition{Kind: ParentAny},
			StartAttributes: map[string]AttributeDefinition{
				"pi.harness.model": {
					Type:        AttributeString,
					Description: "The model the session starts with.",
					Required:    true,
					Cardinality: CardinalityLow,
				},
				"pi.harness.provider": {
					Type:        AttributeString,
					Description: "The provider the session starts with.",
					Required:    true,
					Cardinality: CardinalityLow,
				},
			},
			EndAttributes: map[string]AttributeDefinition{
				"pi.harness.turns": {
					Type:        AttributeNumber,
					Description: "The model completions the session ran.",
				},
				"pi.harness.tool_calls": {
					Type:        AttributeNumber,
					Description: "The tool calls the session executed.",
				},
			},
			Events: map[string]EventDefinition{
				"pi.harness.turn": {
					Description: "One turn of the agent loop.",
					Attributes: map[string]AttributeDefinition{
						"pi.harness.turn_index": {
							Type:        AttributeNumber,
							Description: "The turn's position in the run.",
							Required:    true,
						},
					},
				},
				"pi.harness.tool_call": {
					Description: "One tool call, by name only. Arguments and output are never recorded.",
					Attributes: map[string]AttributeDefinition{
						"pi.harness.tool": {
							Type:        AttributeString,
							Description: "The tool's name.",
							Required:    true,
							Cardinality: CardinalityLow,
						},
						"pi.harness.tool_error": {
							Type:        AttributeBoolean,
							Description: "Whether the call failed.",
							Required:    true,
						},
						"pi.harness.tool_blocked": {
							Type:        AttributeBoolean,
							Description: "Whether a permission rule blocked the call.",
							Required:    true,
						},
					},
				},
			},
			Status: StatusDefinition{
				Default:   StatusOK,
				ErrorWhen: "The session ends with an unrecovered error.",
			},
		},
		"pi.harness.request": {
			Description: "One model request, from context assembly to the last stream event.",
			Parents:     ParentDefinition{Kind: ParentSpans, Spans: []string{"pi.harness.session"}},
			StartAttributes: map[string]AttributeDefinition{
				"pi.harness.provider": {
					Type:        AttributeString,
					Description: "The provider serving the request.",
					Required:    true,
					Cardinality: CardinalityLow,
				},
				"pi.harness.model": {
					Type:        AttributeString,
					Description: "The requested model.",
					Required:    true,
					Cardinality: CardinalityLow,
				},
				"pi.harness.tools": {
					Type:        AttributeNumber,
					Description: "The tools offered to the model.",
					Required:    true,
				},
			},
			EndAttributes: map[string]AttributeDefinition{
				"pi.harness.stop": {
					Type:        AttributeString,
					Description: "The normalized stop reason.",
					Values: []AttributeValue{
						"stop", "tool_calls", "length", "error", "aborted",
					},
				},
				"pi.harness.usage.input": {
					Type:        AttributeNumber,
					Description: "Uncached input tokens.",
				},
				"pi.harness.usage.output": {
					Type:        AttributeNumber,
					Description: "Output tokens.",
				},
				"pi.harness.usage.cache_read": {
					Type:        AttributeNumber,
					Description: "Cache-read tokens.",
				},
				"pi.harness.usage.cache_write": {
					Type:        AttributeNumber,
					Description: "Cache-write tokens.",
				},
			},
			Events: map[string]EventDefinition{
				"pi.harness.first_chunk": {
					Description: "The first stream event arrived. Content is never recorded.",
					Attributes:  map[string]AttributeDefinition{},
				},
			},
			Status: StatusDefinition{
				Default:   StatusOK,
				ErrorWhen: "The provider returned an error or the stream failed.",
			},
		},
		"pi.harness.tool": {
			Description: "One tool execution.",
			Parents:     ParentDefinition{Kind: ParentSpans, Spans: []string{"pi.harness.session", "pi.harness.request"}},
			StartAttributes: map[string]AttributeDefinition{
				"pi.harness.tool": {
					Type:        AttributeString,
					Description: "The tool's name.",
					Required:    true,
					Cardinality: CardinalityLow,
				},
			},
			EndAttributes: map[string]AttributeDefinition{
				"pi.harness.tool_error": {
					Type:        AttributeBoolean,
					Description: "Whether the call failed.",
				},
				"pi.harness.tool_blocked": {
					Type:        AttributeBoolean,
					Description: "Whether a permission rule blocked the call.",
				},
			},
			Status: StatusDefinition{
				Default:   StatusOK,
				ErrorWhen: "The tool returned an error result.",
			},
		},
		"pi.harness.compaction": {
			Description: "One history compaction.",
			Parents:     ParentDefinition{Kind: ParentSpans, Spans: []string{"pi.harness.session"}},
			StartAttributes: map[string]AttributeDefinition{
				"pi.harness.messages_before": {
					Type:        AttributeNumber,
					Description: "The history length before the compaction.",
					Required:    true,
				},
			},
			EndAttributes: map[string]AttributeDefinition{
				"pi.harness.messages_after": {
					Type:        AttributeNumber,
					Description: "The history length after the compaction.",
				},
			},
			Status: StatusDefinition{
				Default:   StatusOK,
				ErrorWhen: "The compaction failed.",
			},
		},
	},
	Metrics: map[string]MetricDefinition{
		"pi.harness.request.duration": {
			Kind:        MetricHistogram,
			Description: "The duration of one model request.",
			Unit:        "ms",
			Boundaries:  []float64{100, 500, 1000, 5000, 15000, 60000},
			Attributes: map[string]AttributeDefinition{
				"pi.harness.provider": {
					Type:        AttributeString,
					Description: "The provider serving the request.",
					Required:    true,
					Cardinality: CardinalityLow,
				},
				"pi.harness.model": {
					Type:        AttributeString,
					Description: "The requested model.",
					Required:    true,
					Cardinality: CardinalityLow,
				},
			},
		},
		"pi.harness.first_chunk.duration": {
			Kind:        MetricHistogram,
			Description: "The time to the first stream event of a request.",
			Unit:        "ms",
			Boundaries:  []float64{50, 200, 500, 1000, 5000},
			Attributes: map[string]AttributeDefinition{
				"pi.harness.provider": {
					Type:        AttributeString,
					Description: "The provider serving the request.",
					Required:    true,
					Cardinality: CardinalityLow,
				},
			},
		},
		"pi.harness.tokens": {
			Kind:        MetricCounter,
			Description: "The model token usage.",
			Unit:        "token",
			Attributes: map[string]AttributeDefinition{
				"pi.harness.kind": {
					Type:        AttributeString,
					Description: "The token usage category.",
					Values:      []AttributeValue{"input", "output", "cache_read", "cache_write"},
					Required:    true,
					Cardinality: CardinalityLow,
				},
				"pi.harness.model": {
					Type:        AttributeString,
					Description: "The requested model.",
					Required:    true,
					Cardinality: CardinalityLow,
				},
			},
		},
		"pi.harness.tool.calls": {
			Kind:        MetricCounter,
			Description: "The tool calls executed.",
			Unit:        "call",
			Attributes: map[string]AttributeDefinition{
				"pi.harness.tool": {
					Type:        AttributeString,
					Description: "The tool's name.",
					Required:    true,
					Cardinality: CardinalityLow,
				},
				"pi.harness.tool_error": {
					Type:        AttributeBoolean,
					Description: "Whether the call failed.",
					Required:    true,
					Cardinality: CardinalityLow,
				},
				"pi.harness.tool_blocked": {
					Type:        AttributeBoolean,
					Description: "Whether a permission rule blocked the call.",
					Required:    true,
					Cardinality: CardinalityLow,
				},
			},
		},
	},
}
