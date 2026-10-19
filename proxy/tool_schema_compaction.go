package proxy

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Tool documentation is reduced only when history trimming alone cannot fit
// the fixed request context. A zero cap removes presentation annotations and
// normalizes line endings but leaves descriptions complete. Subsequent caps
// retain both the beginning and end, and explicitly mark any shortened text.
// Validation keywords and literal application data are never removed.
var kiroToolDescriptionCompactionCaps = []int{0, 4096, 2048, 1024, 512, 256}

const kiroShortenedDescriptionNote = "\n[Description shortened to fit upstream input limits.]\n"

type kiroToolContextCompaction struct {
	ToolContextBytesSaved       int
	CompactedToolDescriptions   int
	CompactedSchemaDescriptions int
	RemovedSchemaAnnotations    int
}

// compactKiroToolContext returns an isolated candidate. In particular, it does
// not modify schema maps shared with the incoming Claude/OpenAI request, or
// touch tool results, the current request, system instructions, or tool names.
func compactKiroToolContext(payload *KiroPayload, maxDescriptionBytes int) (*KiroPayload, kiroToolContextCompaction, error) {
	var stats kiroToolContextCompaction
	if payload == nil {
		return payload, stats, nil
	}
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context == nil || len(context.Tools) == 0 {
		return payload, stats, nil
	}
	wire, err := marshalUpstreamJSON(context.Tools)
	if err != nil {
		return nil, stats, err
	}
	var tools []KiroToolWrapper
	decoder := json.NewDecoder(bytes.NewReader(wire))
	// Schema numbers may exceed float64's exact range. Preserve their original
	// JSON representation, including defaults, bounds, consts, and enum values.
	decoder.UseNumber()
	if err := decoder.Decode(&tools); err != nil {
		return nil, stats, err
	}
	// JSON Pointer references may explicitly use annotation values as schemas
	// (e.g. #/examples/0). Preserve that schema in this unusual case, or the
	// entire tool set for external references, which may target a nested $id
	// in another schema's annotation subtree even without a URI fragment.
	// Tool descriptions can still be shortened independently. Anchored refs
	// are similarly conservative because no pointer identifies their target.
	preserveAllSchemaAnnotations := false
	for i := range tools {
		if kiroSchemaReferencesAnnotations(tools[i].ToolSpecification.InputSchema.JSON, true) {
			preserveAllSchemaAnnotations = true
			break
		}
	}
	for i := range tools {
		specification := &tools[i].ToolSpecification
		if compacted := compactKiroDescription(specification.Description, maxDescriptionBytes); compacted != specification.Description {
			specification.Description = compacted
			stats.CompactedToolDescriptions++
		}
		if !preserveAllSchemaAnnotations && !kiroSchemaReferencesAnnotations(specification.InputSchema.JSON, false) {
			compactKiroSchemaAnnotations(specification.InputSchema.JSON, maxDescriptionBytes, &stats)
		}
	}
	compactedWire, err := marshalUpstreamJSON(tools)
	if err != nil {
		return nil, stats, err
	}
	stats.ToolContextBytesSaved = positiveDifference(len(wire), len(compactedWire))
	if stats.ToolContextBytesSaved == 0 {
		return payload, kiroToolContextCompaction{}, nil
	}
	compactedContext := *context
	compactedContext.Tools = tools
	candidate := *payload
	candidate.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext = &compactedContext
	return &candidate, stats, nil
}

func kiroSchemaReferencesAnnotations(value interface{}, externalOnly bool) bool {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, child := range typed {
			if key == "$ref" || key == "$dynamicRef" || key == "$recursiveRef" {
				if reference, ok := child.(string); ok {
					parsed, err := url.Parse(reference)
					if err != nil {
						return true
					}
					if parsed.Scheme != "" || parsed.Host != "" || parsed.Path != "" {
						return true
					}
					if externalOnly {
						continue
					}
					fragment := parsed.Fragment
					if fragment != "" && !strings.HasPrefix(fragment, "/") {
						return true
					}
					for _, segment := range strings.Split(fragment, "/") {
						segment = strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
						if segment == "description" || segment == "title" || segment == "examples" || segment == "$comment" {
							return true
						}
					}
				}
			}
			if kiroSchemaReferencesAnnotations(child, externalOnly) {
				return true
			}
		}
	case []interface{}:
		for _, child := range typed {
			if kiroSchemaReferencesAnnotations(child, externalOnly) {
				return true
			}
		}
	}
	return false
}

func compactKiroDescription(description string, maxBytes int) string {
	// Preserve Markdown/code indentation and internal whitespace: collapsing
	// arbitrary whitespace can change tool instructions and literal examples.
	description = strings.ReplaceAll(description, "\r\n", "\n")
	if maxBytes <= len(kiroShortenedDescriptionNote)+8 || len(description) <= maxBytes {
		return description
	}
	remaining := maxBytes - len(kiroShortenedDescriptionNote)
	// Tool usage guidance usually starts at the beginning; retain a tail as
	// well so terminal cautions and return-value notes are not all discarded.
	prefixLength := remaining * 3 / 4
	for prefixLength > 0 && !utf8.RuneStart(description[prefixLength]) {
		prefixLength--
	}
	suffixStart := len(description) - (remaining - prefixLength)
	for suffixStart < len(description) && !utf8.RuneStart(description[suffixStart]) {
		suffixStart++
	}
	return description[:prefixLength] + kiroShortenedDescriptionNote + description[suffixStart:]
}

func compactKiroSchemaAnnotations(value interface{}, maxDescriptionBytes int, stats *kiroToolContextCompaction) {
	schema, ok := value.(map[string]interface{})
	if !ok {
		return // Boolean schemas have no annotations.
	}
	for _, keyword := range []string{"title", "examples", "$comment"} {
		if _, present := schema[keyword]; present {
			delete(schema, keyword)
			stats.RemovedSchemaAnnotations++
		}
	}
	if description, ok := schema["description"].(string); ok {
		if compacted := compactKiroDescription(description, maxDescriptionBytes); compacted != description {
			schema["description"] = compacted
			stats.CompactedSchemaDescriptions++
		}
	}
	// Only recognized schema locations are visited. A recursive walk of every
	// JSON object would corrupt application properties called "description",
	// "title", etc., and literal objects inside enum/default/const/examples.
	for _, keyword := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
		if schemas, ok := schema[keyword].(map[string]interface{}); ok {
			for _, child := range schemas {
				compactKiroSchemaAnnotations(child, maxDescriptionBytes, stats)
			}
		}
	}
	for _, keyword := range []string{
		"additionalProperties", "unevaluatedProperties", "additionalItems", "unevaluatedItems",
		"contains", "propertyNames", "not", "if", "then", "else", "contentSchema",
	} {
		compactKiroSchemaAnnotations(schema[keyword], maxDescriptionBytes, stats)
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		if schemas, ok := schema[keyword].([]interface{}); ok {
			for _, child := range schemas {
				compactKiroSchemaAnnotations(child, maxDescriptionBytes, stats)
			}
		}
	}
	// Draft-04 supports tuple arrays in items and schema-valued dependencies.
	if schemas, ok := schema["items"].([]interface{}); ok {
		for _, child := range schemas {
			compactKiroSchemaAnnotations(child, maxDescriptionBytes, stats)
		}
	} else {
		compactKiroSchemaAnnotations(schema["items"], maxDescriptionBytes, stats)
	}
	if dependencies, ok := schema["dependencies"].(map[string]interface{}); ok {
		for _, child := range dependencies {
			compactKiroSchemaAnnotations(child, maxDescriptionBytes, stats)
		}
	}
}
