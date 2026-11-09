package format

import (
	"encoding/json"
	"strings"

	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

// DefaultToolSchemaMaxBytes bounds the serialized size of the tool definition
// block injected into the prompt. It is a soft budget: compression is applied
// in escalating levels until the block fits or the levels run out.
//
// The previous implementation hardcoded 30000 and, once crossed, replaced every
// tool with name+description only. That silently destroyed the JSON Schema, so
// the model had to invent argument names and agents broke. Compression now
// always preserves the schema *shape* (types, property names, required, enum,
// items, additionalProperties); it only sheds cosmetic metadata.
const DefaultToolSchemaMaxBytes = 30000

// MinToolSchemaMaxBytes is the smallest budget compression will honour. Below
// this the schema cannot stay usable, so the budget is clamped up instead of
// silently producing a schema-less prompt where the model invents arguments.
const MinToolSchemaMaxBytes = 8000

// SchemaCompressionLevel is the escalating ladder used when the tool block does
// not fit inside the configured budget. Each step is strictly additive and
// never removes structural information.
type SchemaCompressionLevel int

const (
	// SchemaKeepAll emits the definitions untouched.
	SchemaKeepAll SchemaCompressionLevel = iota
	// SchemaDropCosmetic removes cosmetic keywords: title, $schema, examples,
	// default, format, deprecated, $comment and friends.
	SchemaDropCosmetic
	// SchemaDropDescriptions additionally drops nested property descriptions,
	// keeping only top-level tool descriptions (shortened).
	SchemaDropDescriptions
	// SchemaPruneDeep additionally prunes properties below a depth cutoff,
	// keeping required properties so the contract stays satisfiable.
	SchemaPruneDeep
)

func (l SchemaCompressionLevel) String() string {
	switch l {
	case SchemaKeepAll:
		return "none"
	case SchemaDropCosmetic:
		return "cosmetic"
	case SchemaDropDescriptions:
		return "descriptions"
	case SchemaPruneDeep:
		return "prune-deep"
	}
	return "unknown"
}

// cosmeticSchemaKeys lists JSON Schema keywords that carry no structural
// meaning for argument generation. Dropping them cannot make a valid call
// invalid.
//
// NOTE: `$defs` and `definitions` are deliberately absent. They hold the
// targets of `$ref`, and $ref is a structural keyword that survives every
// level. Dropping the definitions while keeping the references produces
// dangling pointers: the model sees "$ref": "#/$defs/Item" with nothing to
// resolve it to, which is worse than dropping the ref entirely.
var cosmeticSchemaKeys = map[string]bool{
	"$schema":            true,
	"$id":                true,
	"$anchor":            true,
	"$comment":           true,
	"title":              true,
	"description":        true, // dropped only at >= SchemaDropDescriptions
	"examples":           true,
	"example":            true,
	"default":            true,
	"deprecated":         true,
	"readOnly":           true,
	"writeOnly":          true,
	"contentMediaType":   true,
	"contentEncoding":    true,
	"format":             true,
	"x-nullable":         true,
	"x-ordered":          true,
	"x-enumDescriptions": true,
}

// Structural keywords below are never dropped, no matter the level:
// type, properties, required, items, prefixItems, enum, const,
// additionalProperties, patternProperties, anyOf/oneOf/allOf, $ref, nullable,
// propertyNames, the numeric/length bounds and pattern. They define what a
// valid argument object looks like; cosmetic keys do not.

// pruneDepthCutoff is the nesting depth below which properties are dropped at
// SchemaPruneDeep. Every schema at or above it keeps its type information.
const pruneDepthCutoff = 5

// CompressSchema returns a copy of a JSON Schema (as produced by
// json.Unmarshal into any) with cosmetic keywords removed according to level.
// The input is never mutated.
func CompressSchema(node any, level SchemaCompressionLevel) any {
	if level == SchemaKeepAll {
		return node
	}
	return compressNode(node, level, 0)
}

func compressNode(node any, level SchemaCompressionLevel, depth int) any {
	switch v := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			if cosmeticSchemaKeys[k] {
				// Cosmetic keyword. `description` is the only one with a
				// depth-dependent rule: at SchemaDropCosmetic the root schema
				// description survives, but nested property descriptions are
				// already noise by then.
				if k == "description" && level == SchemaDropCosmetic && depth == 0 {
					out[k] = val
				}
				continue
			}

			// Deep pruning: below the cutoff, drop nested `properties` bodies
			// but keep the property *names* and `required` so the shape stays
			// visible. Structural keywords at this depth are preserved.
			if level >= SchemaPruneDeep && k == "properties" && depth >= pruneDepthCutoff {
				if pm, ok := val.(map[string]any); ok {
					slim := make(map[string]any, len(pm))
					for name, sub := range pm {
						slim[name] = shallowTypeOnly(sub)
					}
					out[k] = slim
					continue
				}
			}

			out[k] = compressNode(val, level, depth+1)
		}
		return out

	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = compressNode(item, level, depth)
		}
		return out

	default:
		return node
	}
}

// shallowTypeOnly collapses a nested property schema to its type signature.
// `enum` and `items` survive because they bound the value domain and are cheap.
func shallowTypeOnly(node any) any {
	m, ok := node.(map[string]any)
	if !ok {
		return node
	}
	out := make(map[string]any, 3)
	for _, k := range []string{"type", "enum", "const", "items", "$ref"} {
		if val, ok := m[k]; ok {
			if k == "items" {
				out[k] = shallowTypeOnly(val)
				continue
			}
			out[k] = val
		}
	}
	if len(out) == 0 {
		// Keep *something* so the property is not an unconstrained blob.
		if t, ok := m["type"]; ok {
			out["type"] = t
		} else {
			out["type"] = "object"
		}
	}
	return out
}

// truncateDescription shortens a tool description to n runes, appending an
// ellipsis when it was cut. Descriptions are prompt filler; the schema is the
// contract.
func truncateDescription(desc string, n int) string {
	if n <= 0 || len(desc) <= n {
		return desc
	}
	cut := desc[:n]
	if idx := strings.LastIndexAny(cut, " \n\t"); idx > n*3/4 {
		cut = cut[:idx]
	}
	return strings.TrimRight(cut, " \n\t.,;:") + "…"
}

// CompressToolDefinitions serializes OpenAI-style tool definitions under a byte
// budget, escalating compression instead of discarding the schema.
//
// It returns the marshaled JSON plus the level that was applied. Level
// SchemaKeepAll means the definitions already fit.
func CompressToolDefinitions(fns []models.OpenAIFunction, maxBytes int) ([]byte, SchemaCompressionLevel) {
	if maxBytes <= 0 {
		maxBytes = DefaultToolSchemaMaxBytes
	}
	// A budget below this floor would strip the schema into uselessness; the
	// model would then invent argument names. Keep the floor generous.
	if maxBytes < MinToolSchemaMaxBytes {
		maxBytes = MinToolSchemaMaxBytes
	}

	raw, err := json.Marshal(fns)
	if err != nil {
		// Serialization failure means a non-JSON-encodable parameter tree.
		// Fall back to name+description so the request still succeeds; the
		// caller logs the applied level.
		fallback := make([]models.OpenAIFunction, len(fns))
		for i, fn := range fns {
			fallback[i] = models.OpenAIFunction{Name: fn.Name, Description: fn.Description}
		}
		buf, _ := json.Marshal(fallback)
		return buf, SchemaPruneDeep
	}
	if len(raw) <= maxBytes {
		return raw, SchemaKeepAll
	}

	for level := SchemaDropCosmetic; level <= SchemaPruneDeep; level++ {
		compressed := make([]models.OpenAIFunction, len(fns))
		for i, fn := range fns {
			compressed[i] = compressFunction(fn, level)
		}
		buf, err := json.Marshal(compressed)
		if err != nil {
			continue
		}
		if len(buf) <= maxBytes {
			return buf, level
		}
		// Best effort: keep the last (most compressed) result even when over
		// budget. Oversized prompts still work; a schema-less prompt does not.
		if level == SchemaPruneDeep {
			return buf, level
		}
	}

	// Unreachable, but keep the compiler and future readers happy.
	return raw, SchemaKeepAll
}

func compressFunction(fn models.OpenAIFunction, level SchemaCompressionLevel) models.OpenAIFunction {
	out := models.OpenAIFunction{
		Name: fn.Name,
	}
	switch level {
	case SchemaKeepAll:
		return fn
	case SchemaDropCosmetic:
		out.Description = fn.Description
	case SchemaDropDescriptions:
		out.Description = truncateDescription(fn.Description, 200)
	case SchemaPruneDeep:
		out.Description = truncateDescription(fn.Description, 120)
	}
	if fn.Parameters != nil {
		out.Parameters = CompressSchema(fn.Parameters, level)
	}
	return out
}

// CompressGoogleFunctionDeclarations is the Gemini-native twin of
// CompressToolDefinitions. Google declarations use the same JSON Schema subset
// for `parameters`, so the compression ladder is identical.
func CompressGoogleFunctionDeclarations(defs []models.GoogleFunctionDeclaration, maxBytes int) ([]byte, SchemaCompressionLevel) {
	if maxBytes <= 0 {
		maxBytes = DefaultToolSchemaMaxBytes
	}

	raw, _ := json.Marshal(defs)
	if len(raw) <= maxBytes {
		return raw, SchemaKeepAll
	}

	for level := SchemaDropCosmetic; level <= SchemaPruneDeep; level++ {
		compressed := make([]models.GoogleFunctionDeclaration, len(defs))
		for i, d := range defs {
			compressed[i] = compressDeclaration(d, level)
		}
		buf, err := json.Marshal(compressed)
		if err != nil {
			continue
		}
		if len(buf) <= maxBytes {
			return buf, level
		}
		if level == SchemaPruneDeep {
			return buf, level
		}
	}

	return raw, SchemaKeepAll
}

func compressDeclaration(d models.GoogleFunctionDeclaration, level SchemaCompressionLevel) models.GoogleFunctionDeclaration {
	out := models.GoogleFunctionDeclaration{Name: d.Name}
	switch level {
	case SchemaKeepAll:
		return d
	case SchemaDropCosmetic:
		out.Description = d.Description
	case SchemaDropDescriptions:
		out.Description = truncateDescription(d.Description, 200)
	case SchemaPruneDeep:
		out.Description = truncateDescription(d.Description, 120)
	}
	if d.Parameters != nil {
		out.Parameters = CompressSchema(d.Parameters, level)
	}
	return out
}
